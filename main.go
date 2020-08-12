// Copyright (c) 2017, Randy Westlund. All rights reserved.
// This code is under the BSD-2-Clause license.

// Package gotex is a simple library to render LaTeX documents.
//
// Example
//
// Use it like this:
//
//	package main
//
//	import "github.com/rwestlund/gotex"
//
//	func main() {
//	    var document = `
//	        \documentclass[12pt]{article}
//	        \begin{document}
//	        This is a LaTeX document.
//	        \end{document}
//	        `
//	    var pdf, err = gotex.Render(document, gotex.Options{
//			Command: "/usr/bin/pdflatex",
//			Runs: 1,
//			Texinputs:"/my/asset/dir:/my/other/asset/dir"})
//
//	    if err != nil {
//	        log.Println("render failed ", err)
//	    } else {
//	        // Do something with the PDF file, like send it to an HTTP client
//	        // or write it to a file.
//	        sendSomewhere(pdf)
//	    }
//	}
package gotex

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"

	"github.com/pkg/errors"
)

type LogLevel string

const (
	LVL_DEBUG LogLevel = "DBG"
	LVL_INFO  LogLevel = "INF"
	LVL_WARN  LogLevel = "WRN"
	LVL_ERROR LogLevel = "ERR"
)

type LogFun func(lvl LogLevel, formatStr string, args ...interface{})

var NopLogger = func(lvl LogLevel, formatStr string, args ...interface{}) {}

type TexToPDF interface {
	RenderToFile(document io.Reader, outFilename string) error
}

// Option represents an option for configuration of texToPDFImpl
type Option func(tpdf *texToPDFImpl)

// PdfLatexBin specify the binary that shall be used to generate the pdf (full path to pdflatex)
func PdfLatexBin(cmd string) Option {
	return func(tpdf *texToPDFImpl) {
		tpdf.command = cmd
	}
}

// Runs specifies the number of rounds the tex to pdf rendering shall be called
// A value of 0 (default) will result in a mode where the needed runs are detected automatically.
func Runs(runs int) Option {
	return func(tpdf *texToPDFImpl) {
		tpdf.runs = runs
	}
}

func Logger(logFun LogFun) Option {
	return func(tpdf *texToPDFImpl) {
		tpdf.logger = logFun
	}
}

// TexInputs is a list of folders that contain assets needed to compile the tex file.
// For example images, style sheets, ...
func TexInputs(inputs ...string) Option {
	return func(tpdf *texToPDFImpl) {
		tpdf.texinputs = strings.Join(inputs, ":")
	}
}

// Verbose activates verbose mode where the whole tex log will be printed.
func Verbose() Option {
	return func(tpdf *texToPDFImpl) {
		tpdf.verbose = true
	}
}

type texToPDFImpl struct {
	// Command is the executable to run. It defaults to "pdflatex". Set this to
	// a full path if $PATH will not be defined in your app's environment.
	command string
	// Runs determines how many times Command is run. This is needed for
	// documents that use refrences and packages that require multiple passes.
	// If 0, gotex will automagically attempt to determine how many runs are
	// required by parsing LaTeX log output.
	runs int

	// Texinputs is a colon-separated list of directories containing assests
	// such as image files that are needed to compile the document. It is added
	// to $TEXINPUTS for the LaTeX process.
	texinputs string

	logger LogFun

	jobname string

	verbose bool
}

func New(options ...Option) TexToPDF {

	currentDir, err := os.Getwd()
	if err != nil {
		currentDir = "./"
	}

	tex := texToPDFImpl{
		command:   "pdflatex",
		runs:      0,
		texinputs: currentDir,
		jobname:   "gotex",
		logger:    NopLogger,
		verbose:   false,
	}

	// apply the options
	for _, opt := range options {
		opt(&tex)
	}

	return tex
}

func (tpdf texToPDFImpl) logInfo(formatStr string, args ...interface{}) {
	tpdf.logger(LVL_INFO, formatStr, args...)
}

func (tpdf texToPDFImpl) logDebug(formatStr string, args ...interface{}) {
	tpdf.logger(LVL_DEBUG, formatStr, args...)
}

func (tpdf texToPDFImpl) logWarn(formatStr string, args ...interface{}) {
	tpdf.logger(LVL_WARN, formatStr, args...)
}

func (tpdf texToPDFImpl) logError(formatStr string, args ...interface{}) {
	tpdf.logger(LVL_ERROR, formatStr, args...)
}

func (tpdf texToPDFImpl) RenderToFile(document io.Reader, outFilename string) error {
	tpdf.logInfo("Start Rendering tex to %s", outFilename)

	dir, err := ioutil.TempDir("", fmt.Sprintf("%s-", tpdf.jobname))
	if err != nil {
		return errors.Wrap(err, "Creating temp dir")
	}
	defer os.RemoveAll(dir)

	tpdf.logInfo("Temp dir generated at %s", dir)

	if err := tpdf.renderDocument(document, dir); err != nil {
		return errors.Wrap(err, "Rendering document")
	}

	generatedFile := path.Join(dir, fmt.Sprintf("%s.pdf", tpdf.jobname))
	tpdf.logInfo("PDF successfully generated at %s", generatedFile)

	err = os.Rename(generatedFile, outFilename)
	if err != nil {
		return errors.Wrap(err, "Moving generated pdf to target")
	}

	tpdf.logInfo("PDF %s moved to %s", generatedFile, outFilename)
	return nil
}

func (tpdf texToPDFImpl) renderDocument(document io.Reader, outDir string) error {

	// Unless a number was given, don't let automagic mode run more than this
	// many times.
	maxRuns := 5
	if tpdf.runs > 0 {
		maxRuns = tpdf.runs
	}

	// read the full document into memory
	// this is needed to create a new io.Reader for each of (potentially) multiple runs
	buf, err := ioutil.ReadAll(document)
	if err != nil {
		return errors.Wrap(err, "Reading file content")
	}

	// Keep running until the document is finished or we hit an arbitrary limit.
	runs := 0
	for rerun := true; rerun && runs < maxRuns; runs++ {
		tpdf.logInfo("Compile round #%d", runs)
		document = bytes.NewReader(buf)
		if err := tpdf.runLatex(document, outDir); err != nil {
			return errors.Wrap(err, "Compile tex to pdf")
		}
		// If in automagic mode, determine whether we need to run again.
		if tpdf.runs == 0 {
			rerun, err = needsRerun(outDir, tpdf.jobname)
			if err != nil {
				return err
			}

		}
	}
	return nil
}

// runLatex does the actual work of spawning the child and waiting for it.
func (tpdf texToPDFImpl) runLatex(document io.Reader, dir string) error {
	args := []string{"-halt-on-error", fmt.Sprintf("-jobname=%s", tpdf.jobname)}

	// Prepare the command.
	cmd := exec.Command(tpdf.command, args...)
	// Set the cwd to the temporary directory; LaTeX will write all files there.
	cmd.Dir = dir
	// Feed the document to LaTeX over stdin.
	cmd.Stdin = document

	// Set $TEXINPUTS if requested. The trailing colon means that LaTeX should
	// include the normal asset directories as well.
	if tpdf.texinputs != "" {
		cmd.Env = append(os.Environ(), "TEXINPUTS="+tpdf.texinputs+":")
	}

	// Launch and let it finish.
	err := cmd.Start()
	if err != nil {
		return err
	}
	err = cmd.Wait()

	// print the whole tex log in verbose mode
	if tpdf.verbose {
		if err := tpdf.printLogFie(dir, tpdf.jobname); err != nil {
			return errors.Wrap(err, "Printing log file")
		}
	}

	if err != nil {
		// The actual error is useless, do provide a better one from the logfile
		return getMergedError(dir, tpdf.jobname)
	}

	return nil
}

func (tpdf texToPDFImpl) printLogFie(texWorkingDir string, jobname string) error {
	logfile := path.Join(texWorkingDir, fmt.Sprintf("%s.log", jobname))
	file, err := os.Open(logfile)
	if err != nil {
		return errors.Wrapf(err, "Opening logfile %s", logfile)
	}
	defer file.Close()

	logfileScanner := bufio.NewScanner(file)

	for logfileScanner.Scan() {
		logline := logfileScanner.Text()
		tpdf.logDebug(logline)
	}

	return nil
}

func getMergedError(texWorkingDir string, jobname string) error {
	logfile := path.Join(texWorkingDir, fmt.Sprintf("%s.log", jobname))
	errs, err := getErrorsFromLog(logfile)
	if err != nil {
		return errors.Wrap(err, "Get errors from pdflatex log")
	}
	if len(errs) == 0 {
		return fmt.Errorf("No error found even though pdflatex stopped with an error. Something bad happened")
	}

	return fmt.Errorf("%s", strings.Join(errs, "|"))
}

func getErrorsFromLog(logfile string) ([]string, error) {

	matcher, err := regexp.Compile("(^!.*|^<\\*>)")
	if err != nil {
		return nil, errors.Wrap(err, "Compile regex matcher for errors in log")
	}

	file, err := os.Open(logfile)
	if err != nil {
		return nil, errors.Wrapf(err, "Opening logfile %s", logfile)
	}
	defer file.Close()

	errs := make([]string, 0)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		logline := scanner.Text()
		if matcher.MatchString(logline) {
			errs = append(errs, strings.TrimSpace(logline))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.Wrapf(err, "Reading logfile %s", logfile)
	}
	return errs, nil
}

// Parse the log file and attempt to determine whether another run is necessary
// to finish the document.
func needsRerun(dir string, jobname string) (bool, error) {
	file, err := os.Open(path.Join(dir, fmt.Sprintf("%s.log", jobname)))
	if err != nil {
		return false, errors.Wrap(err, "Open log file")
	}
	defer file.Close()

	matcher, err := regexp.Compile(".*Rerun to get.*")
	if err != nil {
		return false, errors.Wrap(err, "Compile regex matcher for check for needed rerun")
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// Look for a line like:
		// "Label(s) may have changed. Rerun to get cross-references right."
		if matcher.MatchString(scanner.Text()) {
			return true, nil
		}
	}
	return false, nil
}
