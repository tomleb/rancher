package main

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// webhookDocFileName defines the name of the files that will be aggregated into overall docs
const webhookDocFileExtension = ".md"

type webhookDocFile struct {
	content  []byte
	resource string
	group    string
	version  string
}

func generateWebhookDocs(resourcesBaseDir, outputFilePath string) (err error) {
	outputFile, err := os.OpenFile(outputFilePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	defer func() {
		closeErr := outputFile.Close()
		if closeErr != nil {
			if err != nil {
				err = fmt.Errorf("%w, error when closing file %s", err, closeErr.Error())
			} else {
				err = closeErr
			}
		}
	}()
	if err != nil {
		return err
	}

	webhookDocFiles, err := getWebhookDocFiles(resourcesBaseDir)
	if err != nil {
		return fmt.Errorf("unable to create documentation: %w", err)
	}

	currentGroup := ""
	for _, webhookDocFile := range webhookDocFiles {
		newGroup := webhookDocFile.group
		if newGroup != currentGroup {
			// our group has changed, output a new group header
			groupFormatString := "# %s/%s\n"
			if currentGroup != "" {
				groupFormatString = "\n" + groupFormatString
			}
			_, err = fmt.Fprintf(outputFile, groupFormatString, webhookDocFile.group, webhookDocFile.version)
			if err != nil {
				return fmt.Errorf("unable to write group header for %s/%s: %w", webhookDocFile.group, webhookDocFile.version, err)
			}
			currentGroup = newGroup
		}

		_, err = fmt.Fprintf(outputFile, "\n## %s\n\n", webhookDocFile.resource)
		if err != nil {
			return fmt.Errorf("unable to write resource header for %s: %w", webhookDocFile.resource, err)
		}

		scanner := bufio.NewScanner(bytes.NewReader(webhookDocFile.content))
		for scanner.Scan() {
			line := scanner.Bytes()
			// even if the scanned line is empty, still need to output the newline
			if len(line) != 0 && line[0] == '#' {
				// this line is a markdown header. Since the group header is the top-level indent, indent this down one line
				line = append([]byte{'#'}, line...)
			}
			line = append(line, byte('\n'))
			_, err := outputFile.Write(line)
			if err != nil {
				return fmt.Errorf("unable to write content for %s/%s.%s: %w", webhookDocFile.group, webhookDocFile.version, webhookDocFile.resource, err)
			}
		}
		if scanner.Err() != nil {
			return fmt.Errorf("got an error scanning content for %s/%s.%s: %w", webhookDocFile.group, webhookDocFile.version, webhookDocFile.resource, err)
		}
	}

	return nil
}

// getWebhookDocFiles finds all markdown files recursively in resourcesBaseDir and converts them to webhookDocFiles. Returns in a sorted order,
// first by group, then by resourceName
func getWebhookDocFiles(baseDir string) ([]webhookDocFile, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("unable to list entries in directory %s: %w", baseDir, err)
	}

	var webhookDocFiles []webhookDocFile
	for _, entry := range entries {
		entryPath := filepath.Join(baseDir, entry.Name())
		if entry.IsDir() {
			subDocFiles, err := getWebhookDocFiles(entryPath)
			if err != nil {
				return nil, err
			}
			webhookDocFiles = append(webhookDocFiles, subDocFiles...)
			continue
		}
		if filepath.Ext(entry.Name()) != webhookDocFileExtension {
			continue
		}
		content, err := os.ReadFile(filepath.Join(baseDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("unable to read file content for %s: %w", entryPath, err)
		}
		// lop off the last trailing new line to keep consistent spacing for later on
		if content[len(content)-1] == '\n' {
			content = content[:len(content)-1]
		}
		newDir, _ := filepath.Split(baseDir)
		newDir, version := filepath.Split(newDir[:len(newDir)-1])
		newDir, group := filepath.Split(newDir[:len(newDir)-1])
		resource := strings.TrimSuffix(entry.Name(), webhookDocFileExtension)
		if newDir == "" || resource == "" || version == "" || group == "" {
			return nil, fmt.Errorf("unable to extract gvr from %s, got group %s, version %s, resource %s", baseDir, group, version, resource)
		}
		// group and version need to have a consistent case so that test.cattle.io/v3 and test.cattle.Io/V3 are grouped the same way
		caser := cases.Lower(language.English)
		webhookDocFiles = append(webhookDocFiles, webhookDocFile{
			content:  content,
			resource: resource,
			group:    caser.String(group),
			version:  caser.String(version),
		})
	}
	// if the groups differ, sort based on the group. If the groups are the same, sort based on the resource
	slices.SortFunc(webhookDocFiles, func(a, b webhookDocFile) int {
		if a.group == b.group {
			if a.resource == b.resource {
				return cmp.Compare(a.version, b.version)
			}
			return cmp.Compare(a.resource, b.resource)
		}
		return cmp.Compare(a.group, b.group)
	})

	return webhookDocFiles, nil
}
