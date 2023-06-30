// Copyright (c) 2019-2023, Sylabs Inc. All rights reserved.
// Copyright (c) Contributors to the Apptainer project, established as
//   Apptainer a Series of LF Projects LLC.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package args

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"

	"github.com/samber/lo"
	"github.com/sylabs/singularity/pkg/sylog"
)

type Reader struct {
	src            *bufio.Reader
	buildArgsMap   map[string]string
	defaultArgsMap map[string]string
	consumedArgs   map[string]bool
	buf            bytes.Buffer
	bufReader      io.Reader
	bufWriter      io.Writer
}

const bufSize = 1024

var (
	buildArgsRegexp              = regexp.MustCompile(`{{\s*(\w+)\s*}}`)
	buildArgsIncompleteSrcRegexp = regexp.MustCompile(`{(({\s*((\w+\s*(}?))?))?)$`)
)

func NewReader(src io.Reader, buildArgsMap map[string]string, defaultArgsMap map[string]string) *Reader {
	r := &Reader{
		src:            bufio.NewReader(src),
		buildArgsMap:   buildArgsMap,
		defaultArgsMap: defaultArgsMap,
	}

	r.consumedArgs = make(map[string]bool)
	r.bufReader = io.Reader(&r.buf)
	r.bufWriter = io.Writer(&r.buf)

	return r
}

func (r *Reader) Read(p []byte) (n int, err error) {
	var srcBytes []byte
	var matches [][]int
	atRealEOF := false
	for !atRealEOF {
		srcBufSize := r.src.Buffered()
		sylog.Debugf("Underlying bufio.Reader has %d buffered bytes", srcBufSize)
		if srcBufSize < 1 {
			srcBufSize = bufSize
		}
		newBytes := make([]byte, srcBufSize)
		n, err := r.src.Read(newBytes)
		sylog.Debugf("Got (%d, %v) from underlying bufio.Reader", n, err)
		if err == io.EOF {
			atRealEOF = true
		} else if err != nil {
			return 0, err
		}

		srcBytes = append(srcBytes, newBytes[:n]...)
		matches = buildArgsRegexp.FindAllSubmatchIndex(srcBytes, -1)
		sylog.Debugf("matches are %#v", matches)
		lastMatchEnd := 0
		if len(matches) > 0 {
			lastMatch := matches[len(matches)-1]
			lastMatchEnd = lastMatch[1]
		}

		tail := srcBytes[lastMatchEnd:]
		incomplete := buildArgsIncompleteSrcRegexp.FindIndex(tail)
		if incomplete == nil {
			break
		}

		if !atRealEOF {
			sylog.Debugf("Encountered possible incomplete build-arg: %v", string(tail[incomplete[0]:incomplete[1]]))
			continue
		}
	}

	i := 0
	for _, match := range matches {
		r.bufWriter.Write(srcBytes[i:match[0]])
		argName := string(srcBytes[match[2]:match[3]])
		val, ok := r.buildArgsMap[argName]
		if !ok {
			val, ok = r.defaultArgsMap[argName]
		}
		if !ok {
			return 0, fmt.Errorf("build var %s is not defined through either --build-arg (--build-arg-file) or 'arguments' section", argName)
		}
		r.bufWriter.Write([]byte(val))
		r.consumedArgs[argName] = true
		i = match[1]
	}
	r.bufWriter.Write(srcBytes[i:])

	// Read (up to) len(p) bytes to return to the caller from our internal buffer
	n, err = r.bufReader.Read(p)
	if err == io.EOF && !atRealEOF {
		err = nil
	}

	return n, err
}

func (r Reader) ConsumedArgs() []string {
	return lo.Keys(r.consumedArgs)
}
