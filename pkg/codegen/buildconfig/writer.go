package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

type GoConstantsWriter struct {
	Config map[string]string
	Output io.Writer
	Tmpl   *template.Template
	buf    []byte
}

// Run processes the pre-configured Config and outputs a template with formatted
// Go constants to the pre-configured Output.
func (f *GoConstantsWriter) Run() error {
	if err := f.process(); err != nil {
		return err
	}
	if err := f.write(); err != nil {
		return err
	}
	return nil
}

func (f *GoConstantsWriter) process() error {
	if f.Tmpl == nil {
		return errors.New("nil template")
	}
	if f.Config == nil {
		return errors.New("nil config")
	}
	// This sorts the keys alphabetically to process the map in a fixed order.
	keys := make([]string, 0, len(f.Config))
	for k := range f.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	capitalize := cases.Title(language.English, cases.NoLower)
	// Each constant is a standalone declaration separated by a blank line, rather than an
	// aligned const block. Concurrent PRs bumping different keys then touch non-adjacent
	// lines, and a longer key name doesn't realign (and conflict on) every other line.
	var builder strings.Builder
	for i, k := range keys {
		if i > 0 {
			builder.WriteString("\n")
		}
		v := f.Config[k]
		// Capitalize the key to make the constant exported in the generated Go file.
		k = capitalize.String(k)
		builder.WriteString(fmt.Sprintf("const %s = %q\n", k, v))
	}

	buf := new(bytes.Buffer)
	if err := f.Tmpl.Execute(buf, builder.String()); err != nil {
		return err
	}
	f.buf = buf.Bytes()
	return nil
}

func (f *GoConstantsWriter) write() error {
	if f.Output == nil {
		return errors.New("nil output")
	}
	formatted, err := format.Source(f.buf)
	if err != nil {
		return err
	}
	_, err = f.Output.Write(formatted)
	return err
}
