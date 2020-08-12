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

type LogFun func(logline string)

var NopLogger = func(logline string) {}

// Options contains the knobs used to change gotex's behavior.
type Options struct {
	// Command is the executable to run. It defaults to "pdflatex". Set this to
	// a full path if $PATH will not be defined in your app's environment.
	Command string
	// Runs determines how many times Command is run. This is needed for
	// documents that use refrences and packages that require multiple passes.
	// If 0, gotex will automagically attempt to determine how many runs are
	// required by parsing LaTeX log output.
	Runs int

	// Texinputs is a colon-separated list of directories containing assests
	// such as image files that are needed to compile the document. It is added
	// to $TEXINPUTS for the LaTeX process.
	Texinputs string

	Logger LogFun
}

func RenderToFile(document io.Reader, outFilename string, options Options) error {
	// Set default options.
	if options.Command == "" {
		options.Command = "pdflatex"
	}
	jobname := "gotex"

	dir, err := ioutil.TempDir("", fmt.Sprintf("%s-", jobname))
	if err != nil {
		return errors.Wrap(err, "Creating temp dir")
	}
	defer os.RemoveAll(dir)

	if err := renderDocument(document, dir, jobname, options); err != nil {
		return errors.Wrap(err, "Rendering document")
	}

	generatedFile := path.Join(dir, fmt.Sprintf("%s.pdf", jobname))
	err = os.Rename(generatedFile, outFilename)
	if err != nil {
		return errors.Wrap(err, "moving generated pdf to target")
	}

	return nil
}

// Render takes the LaTeX document to be rendered as a string. It returns the
// resulting PDF as a []byte. If there's an error, Render will leave the
// temporary directory intact so you can check the log file to see what
// happened. The error will tell you where to find it.
func Render(document io.Reader, options Options) ([]byte, error) {
	// Set default options.
	if options.Command == "" {
		options.Command = "pdflatex"
	}
	jobname := "gotex"

	dir, err := ioutil.TempDir("", fmt.Sprintf("%s-", jobname))
	if err != nil {
		return nil, errors.Wrap(err, "Creating temp dir")
	}
	defer os.RemoveAll(dir)

	if err := renderDocument(document, dir, jobname, options); err != nil {
		return nil, errors.Wrap(err, "Rendering document")
	}

	// Slurp the output.
	output, err := ioutil.ReadFile(path.Join(dir, fmt.Sprintf("%s.pdf", jobname)))
	if err != nil {
		return nil, errors.Wrap(err, "read generated pdf into buffer")
	}

	return output, nil
}

func renderDocument(document io.Reader, outDir string, jobname string, options Options) error {
	// Set default options.
	if options.Command == "" {
		options.Command = "pdflatex"
	}

	// Unless a number was given, don't let automagic mode run more than this
	// many times.
	var maxRuns = 5
	if options.Runs > 0 {
		maxRuns = options.Runs
	}

	// read the full document into memory
	// this is needed to create a new io.Reader for each of (potentially) multiple runs
	buf, err := ioutil.ReadAll(document)
	if err != nil {
		return errors.Wrap(err, "Reading file data")
	}

	// Keep running until the document is finished or we hit an arbitrary limit.
	var runs int
	for rerun := true; rerun && runs < maxRuns; runs++ {
		document = bytes.NewReader(buf)
		if err := runLatex(document, options, outDir, jobname); err != nil {
			return errors.Wrap(err, "compile tex to pdf")
		}
		// If in automagic mode, determine whether we need to run again.
		if options.Runs == 0 {
			rerun, err = needsRerun(outDir, jobname)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// runLatex does the actual work of spawning the child and waiting for it.
func runLatex(document io.Reader, options Options, dir string, jobname string) error {
	args := []string{"-halt-on-error", fmt.Sprintf("-jobname=%s", jobname)}

	// Prepare the command.
	cmd := exec.Command(options.Command, args...)
	// Set the cwd to the temporary directory; LaTeX will write all files there.
	cmd.Dir = dir
	// Feed the document to LaTeX over stdin.
	cmd.Stdin = document

	// Set $TEXINPUTS if requested. The trailing colon means that LaTeX should
	// include the normal asset directories as well.
	if options.Texinputs != "" {
		cmd.Env = append(os.Environ(), "TEXINPUTS="+options.Texinputs+":")
	}

	// Launch and let it finish.
	err := cmd.Start()
	if err != nil {
		return err
	}
	err = cmd.Wait()
	if err != nil {
		// The actual error is useless, do provide a better one from the logfile
		return getMergedError(dir, jobname)
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
		return nil, errors.Wrap(err, "compile regex matcher for errors in log")
	}

	file, err := os.Open(logfile)
	if err != nil {
		return nil, errors.Wrapf(err, "opening logfile %s", logfile)
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
		return nil, errors.Wrapf(err, "reading logfile %s", logfile)
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
		return false, errors.Wrap(err, "compile regex matcher for check for needed rerun")
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
