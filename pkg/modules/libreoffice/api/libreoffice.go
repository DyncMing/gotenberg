package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/gotenberg/gotenberg/v8/pkg/gotenberg"
)

type libreOffice interface {
	gotenberg.Process
	pdf(ctx context.Context, logger *zap.Logger, inputPath, outputPath string, options Options) error
}

type libreOfficeArguments struct {
	binPath      string
	unoBinPath   string
	startTimeout time.Duration
}

type libreOfficeProcess struct {
	socketPort         int
	userProfileDirPath string
	cmd                *gotenberg.Cmd
	cfgMu              sync.RWMutex
	isStarted          atomic.Bool

	arguments libreOfficeArguments
	fs        *gotenberg.FileSystem
}

func newLibreOfficeProcess(arguments libreOfficeArguments) libreOffice {
	p := &libreOfficeProcess{
		arguments: arguments,
		fs:        gotenberg.NewFileSystem(new(gotenberg.OsMkdirAll)),
	}
	p.isStarted.Store(false)

	return p
}

func (p *libreOfficeProcess) Start(logger *zap.Logger) error {
	if p.isStarted.Load() {
		return errors.New("LibreOffice is already started")
	}

	port, err := freePort(logger)
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}

	userProfileDirPath := p.fs.NewDirPath()
	args := []string{
		"--headless",
		"--invisible",
		"--nocrashreport",
		"--nodefault",
		"--nologo",
		"--nofirststartwizard",
		"--norestore",
		fmt.Sprintf("-env:UserInstallation=file://%s", userProfileDirPath),
		fmt.Sprintf("--accept=socket,host=127.0.0.1,port=%d,tcpNoDelay=1;urp;StarOffice.ComponentContext", port),
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.arguments.startTimeout)
	defer cancel()

	cmd, err := gotenberg.CommandContext(ctx, logger, p.arguments.binPath, args...)
	if err != nil {
		return fmt.Errorf("create LibreOffice command: %w", err)
	}

	// For whatever reason, LibreOffice requires a first start before being
	// able to run as a daemon.
	//exitCode, err := cmd.Exec()
	//if err != nil && exitCode != 81 {
	//	return fmt.Errorf("execute LibreOffice: %w", err)
	//}

	logger.Debug("got exit code 81, e.g., LibreOffice first start")

	// Second start (daemon).
	cmd = gotenberg.Command(logger, p.arguments.binPath, args...)

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start LibreOffice: %w", err)
	}

	waitChan := make(chan error, 1)

	go func() {
		// By waiting the process, we avoid the creation of a zombie process
		// and make sure we catch an early exit if any.
		waitChan <- cmd.Wait()
	}()

	connChan := make(chan error, 1)

	go func() {
		// As the LibreOffice socket may take some time to be available, we
		// have to ensure that it is indeed accepting connections.
		for {
			if ctx.Err() != nil {
				connChan <- ctx.Err()
				break
			}

			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Duration(1)*time.Second)
			if err != nil {
				continue
			}

			connChan <- nil
			err = conn.Close()
			if err != nil {
				logger.Debug(fmt.Sprintf("close connection after health checking the LibreOffice: %v", err))
			}

			break
		}
	}()

	var success bool

	defer func() {
		if success {
			p.cfgMu.Lock()
			defer p.cfgMu.Unlock()

			p.socketPort = port
			p.userProfileDirPath = userProfileDirPath
			p.cmd = cmd
			p.isStarted.Store(true)

			return
		}

		// Let's make sure the process is killed.
		err = cmd.Kill()
		if err != nil {
			logger.Debug(fmt.Sprintf("kill LibreOffice process: %v", err))
		}

		// And the user profile directory is deleted.
		err = os.RemoveAll(userProfileDirPath)
		if err != nil {
			logger.Error(fmt.Sprintf("remove LibreOffice's user profile directory: %v", err))
		}

		logger.Debug(fmt.Sprintf("'%s' LibreOffice's user profile directory removed", userProfileDirPath))
	}()

	logger.Debug("waiting for the LibreOffice socket to be available...")

	for {
		select {
		case err = <-connChan:
			if err != nil {
				return fmt.Errorf("LibreOffice socket not available: %w", err)
			}

			logger.Debug("LibreOffice socket available")
			success = true

			return nil
		case err = <-waitChan:
			return fmt.Errorf("LibreOffice process exited: %w", err)
		}
	}
}

func (p *libreOfficeProcess) Stop(logger *zap.Logger) error {
	if !p.isStarted.Load() {
		// No big deal? Like calling cancel twice.
		return nil
	}

	// Always remove the user profile directory created by LibreOffice.
	copyUserProfileDirPath := p.userProfileDirPath
	expirationTime := time.Now()
	defer func(userProfileDirPath string, expirationTime time.Time) {
		go func() {
			err := os.RemoveAll(userProfileDirPath)
			if err != nil {
				logger.Error(fmt.Sprintf("remove LibreOffice's user profile directory: %v", err))
			} else {
				logger.Debug(fmt.Sprintf("'%s' LibreOffice's user profile directory removed", userProfileDirPath))
			}

			// Also, remove LibreOffice specific files in the temporary directory.
			err = gotenberg.GarbageCollect(logger, os.TempDir(), []string{"OSL_PIPE", ".tmp"}, expirationTime)
			if err != nil {
				logger.Error(err.Error())
			}
		}()
	}(copyUserProfileDirPath, expirationTime)

	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()

	err := p.cmd.Kill()
	if err != nil {
		return fmt.Errorf("kill LibreOffice process: %w", err)
	}

	p.socketPort = 0
	p.userProfileDirPath = ""
	p.cmd = nil
	p.isStarted.Store(false)

	return nil
}

func (p *libreOfficeProcess) Healthy(logger *zap.Logger) bool {
	// Good to know: the supervisor does not call this method if no first start
	// or if the process is restarting.

	if !p.isStarted.Load() {
		// Non-started browser but not restarting?
		return false
	}

	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.socketPort), time.Duration(10)*time.Second)
	if err == nil {
		err = conn.Close()
		if err != nil {
			logger.Debug(fmt.Sprintf("close connection after health checking LibreOffice: %v", err))
		}

		return true
	}

	return false
}

func (p *libreOfficeProcess) pdf(ctx context.Context, logger *zap.Logger, inputPath, outputPath string, options Options) error {
	if !p.isStarted.Load() {
		return errors.New("LibreOffice not started, cannot handle PDF conversion")
	}

	args := []string{
		"--no-launch",
		"--format",
		"pdf",
	}

	args = append(args, "--port", fmt.Sprintf("%d", p.socketPort))

	checkedEntry := logger.Check(zap.DebugLevel, "check for debug level before setting high verbosity")
	if checkedEntry != nil {
		args = append(args, "-vvv")
	}

	if options.Password != "" {
		args = append(args, "--password", options.Password)
	}

	if options.Landscape {
		args = append(args, "--printer", "PaperOrientation=landscape")
	}

	// See: https://github.com/gotenberg/gotenberg/issues/1149.
	if options.PageRanges != "" {
		args = append(args, "--export", fmt.Sprintf("PageRange=%s", options.PageRanges))
	}

	if !options.UpdateIndexes {
		args = append(args, "--disable-update-indexes")
	}

	args = append(args, "--export", fmt.Sprintf("ExportFormFields=%t", options.ExportFormFields))
	args = append(args, "--export", fmt.Sprintf("AllowDuplicateFieldNames=%t", options.AllowDuplicateFieldNames))
	args = append(args, "--export", fmt.Sprintf("ExportBookmarks=%t", options.ExportBookmarks))
	args = append(args, "--export", fmt.Sprintf("ExportBookmarks=%t", options.ExportBookmarks))
	args = append(args, "--export", fmt.Sprintf("ExportBookmarksToPDFDestination=%t", options.ExportBookmarksToPdfDestination))
	args = append(args, "--export", fmt.Sprintf("ExportPlaceholders=%t", options.ExportPlaceholders))
	args = append(args, "--export", fmt.Sprintf("ExportNotes=%t", options.ExportNotes))
	args = append(args, "--export", fmt.Sprintf("ExportNotesPages=%t", options.ExportNotesPages))
	args = append(args, "--export", fmt.Sprintf("ExportOnlyNotesPages=%t", options.ExportOnlyNotesPages))
	args = append(args, "--export", fmt.Sprintf("ExportNotesInMargin=%t", options.ExportNotesInMargin))
	args = append(args, "--export", fmt.Sprintf("ConvertOOoTargetToPDFTarget=%t", options.ConvertOooTargetToPdfTarget))
	args = append(args, "--export", fmt.Sprintf("ExportLinksRelativeFsys=%t", options.ExportLinksRelativeFsys))
	args = append(args, "--export", fmt.Sprintf("ExportHiddenSlides=%t", options.ExportHiddenSlides))
	args = append(args, "--export", fmt.Sprintf("IsSkipEmptyPages=%t", options.SkipEmptyPages))
	args = append(args, "--export", fmt.Sprintf("IsAddStream=%t", options.AddOriginalDocumentAsStream))
	args = append(args, "--export", fmt.Sprintf("SinglePageSheets=%t", options.SinglePageSheets))
	args = append(args, "--export", fmt.Sprintf("UseLosslessCompression=%t", options.LosslessImageCompression))
	args = append(args, "--export", fmt.Sprintf("Quality=%d", options.Quality))
	args = append(args, "--export", fmt.Sprintf("ReduceImageResolution=%t", options.ReduceImageResolution))
	args = append(args, "--export", fmt.Sprintf("MaxImageResolution=%d", options.MaxImageResolution))

	switch options.PdfFormats.PdfA {
	case "":
	case gotenberg.PdfA1b:
		args = append(args, "--export", "SelectPdfVersion=1")
	case gotenberg.PdfA2b:
		args = append(args, "--export", "SelectPdfVersion=2")
	case gotenberg.PdfA3b:
		args = append(args, "--export", "SelectPdfVersion=3")
	default:
		return ErrInvalidPdfFormats
	}

	if options.PdfFormats.PdfUa {
		args = append(
			args,
			"--export", "PDFUACompliance=true",
			"--export", "UseTaggedPDF=true",
			"--export", "EnableTextAccessForAccessibilityTools=true",
		)
	} else {
		args = append(
			args,
			"--export", "PDFUACompliance=false",
			"--export", "UseTaggedPDF=false",
			"--export", "EnableTextAccessForAccessibilityTools=false",
		)
	}

	inputPath, err := nonBasicLatinCharactersGuard(logger, inputPath)
	if err != nil {
		return fmt.Errorf("non-basic latin characters guard: %w", err)
	}

	args = append(args, "--output", outputPath, inputPath)

	cmd, err := gotenberg.CommandContext(ctx, logger, p.arguments.unoBinPath, args...)
	if err != nil {
		return fmt.Errorf("create uno command: %w", err)
	}

	logger.Debug(fmt.Sprintf("print to PDF with: %+v", options))

	exitCode, err := cmd.Exec()
	if err == nil {
		return nil
	}

	// LibreOffice's errors are not explicit.
	// For instance, exit code 5 may be explained by a malformed page range
	// but also by a not required password.

	// We may want to retry in case of a core-dumped event.
	// See https://github.com/gotenberg/gotenberg/issues/639.
	if strings.Contains(err.Error(), "core dumped") {
		return ErrCoreDumped
	}

	if exitCode == 5 {
		// Potentially malformed page ranges or password not required.
		return ErrUnoException
	}
	if exitCode == 6 {
		// Password potentially required or invalid.
		return ErrRuntimeException
	}

	return fmt.Errorf("convert to PDF: %w", err)
}

// LibreOffice cannot convert a file with a name containing non-basic Latin
// characters.
// See:
// https://github.com/gotenberg/gotenberg/issues/104
// https://github.com/gotenberg/gotenberg/issues/730
func nonBasicLatinCharactersGuard(logger *zap.Logger, inputPath string) (string, error) {
	hasNonBasicLatinChars := func(str string) bool {
		for _, r := range str {
			// Check if the character is outside basic Latin.
			if r != '.' && (r < ' ' || r > '~') {
				return true
			}
		}
		return false
	}

	filename := filepath.Base(inputPath)
	if !hasNonBasicLatinChars(filename) {
		logger.Debug("no non-basic latin characters in filename, skip copy")
		return inputPath, nil
	}

	logger.Warn("non-basic latin characters in filename, copy to a file with a valid filename")
	basePath := filepath.Dir(inputPath)
	ext := filepath.Ext(inputPath)
	newInputPath := filepath.Join(basePath, fmt.Sprintf("%s%s", uuid.NewString(), ext))

	in, err := os.Open(inputPath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}

	defer func() {
		err := in.Close()
		if err != nil {
			logger.Error(fmt.Sprintf("close file: %s", err))
		}
	}()

	out, err := os.Create(newInputPath)
	if err != nil {
		return "", fmt.Errorf("create new file: %w", err)
	}

	defer func() {
		err := out.Close()
		if err != nil {
			logger.Error(fmt.Sprintf("close new file: %s", err))
		}
	}()

	_, err = io.Copy(out, in)
	if err != nil {
		return "", fmt.Errorf("copy file to new file: %w", err)
	}

	return newInputPath, nil
}

// Interface guards.
var (
	_ gotenberg.Process = (*libreOfficeProcess)(nil)
	_ libreOffice       = (*libreOfficeProcess)(nil)
)
