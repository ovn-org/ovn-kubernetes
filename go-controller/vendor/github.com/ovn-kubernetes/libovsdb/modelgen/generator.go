package modelgen

import (
	"bytes"
	"fmt"
	"go/format"
	"log"
	"os"
	"text/template"
)

// Generator is an interface that allows to format code from a template and write it to a file
type Generator interface {
	Generate(string, *template.Template, any) error
	Format(*template.Template, any) ([]byte, error)
}

type generator struct {
	dryRun bool
}

// Format returns a formatted byte slice by executing the template with the given args
func (g *generator) Format(tmpl *template.Template, args any) ([]byte, error) {
	buffer := bytes.Buffer{}
	err := tmpl.Execute(&buffer, args)
	if err != nil {
		return nil, err
	}

	src, err := format.Source(buffer.Bytes())
	if err != nil {
		return nil, err
	}
	return src, nil
}

// Generate generates the code and writes it to specified file path
func (g *generator) Generate(filename string, tmpl *template.Template, args any) error {
	src, err := g.Format(tmpl, args)
	if err != nil {
		return err
	}

	if g.dryRun {
		log.Printf("---- Content of file %s ----\n", filename)
		log.Print(string(src))
		fmt.Print("\n")
		return nil
	}
	content, err := os.ReadFile(filename)
	if err == nil && bytes.Equal(content, src) {
		return nil
	}
	return os.WriteFile(filename, src, 0644)
}

// NewGenerator returns a new Generator
func NewGenerator(opts ...Option) (Generator, error) {
	options, err := newOptions(opts...)
	if err != nil {
		return nil, err
	}
	return &generator{
		dryRun: options.dryRun,
	}, nil
}
