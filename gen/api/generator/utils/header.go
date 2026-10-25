package utils

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Header builds the header of a generated file.
//
// The build constraint comes first so the compiler skips the file, then the
// boilerplate. The boilerplate is copied verbatim apart from the YEAR
// placeholder. gengo/v2's own boilerplate helper appends a newline of its own
// to the boilerplate file, which would add a blank line between the header and
// the package clause of every generated file.
func Header(buildTag, headerFile string) ([]byte, error) {
	boilerplate, err := os.ReadFile(headerFile)
	if err != nil {
		return nil, fmt.Errorf("read boilerplate %q: %w", headerFile, err)
	}

	boilerplate = bytes.ReplaceAll(boilerplate, []byte("YEAR"), []byte(strconv.Itoa(time.Now().UTC().Year())))

	return append([]byte(fmt.Sprintf("//go:build !%s\n// +build !%s\n\n", buildTag, buildTag)), boilerplate...), nil
}
