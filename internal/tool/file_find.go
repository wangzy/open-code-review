package tool

import (
	"context"
	"strings"
)

const fileFindMaxCount = 100

// FileFindProvider finds files by name or pattern in the repository.
type FileFindProvider struct {
	FileReader *FileReader
}

func NewFileFind(fr *FileReader) *FileFindProvider { return &FileFindProvider{FileReader: fr} }

func (p *FileFindProvider) Tool() Tool { return FileFind }

func (p *FileFindProvider) Execute(ctx context.Context, args map[string]any) (string, error) {
	queryName, _ := args["query_name"].(string)
	if strings.TrimSpace(queryName) == "" {
		return "// The file was not found", nil
	}

	caseSensitive, _ := args["case_sensitive"].(bool)

	files, err := p.FileReader.Runner.ListFiles(ctx)
	if err != nil {
		return "", err
	}

	var matched []string
	for _, f := range files {
		base := f
		if idx := strings.LastIndex(f, "/"); idx != -1 {
			base = f[idx+1:]
		}
		match := false
		if caseSensitive {
			match = strings.Contains(base, queryName)
		} else {
			match = strings.Contains(strings.ToLower(base), strings.ToLower(queryName))
		}
		if match {
			matched = append(matched, f)
		}
		if len(matched) >= fileFindMaxCount {
			break
		}
	}

	if len(matched) == 0 {
		return "// The file was not found", nil
	}
	return strings.Join(matched, "\n"), nil
}
