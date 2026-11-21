package diffvalidate

import (
	"bufio"
	"strconv"
	"strings"

	"giteye/internal/gitea"
)

type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// DiffLines records new-file line numbers that are part of unified diff hunks.
func DiffLines(diff string) map[string]map[int]struct{} {
	result := map[string]map[int]struct{}{}
	s := bufio.NewScanner(strings.NewReader(diff))
	s.Buffer(make([]byte, 4096), len(diff)+1)
	path, newLine, inHunk := "", 0, false
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "diff --git ") {
			path = ""
			inHunk = false
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			path = strings.TrimPrefix(line, "+++ b/")
			if strings.TrimSpace(line) == "+++ /dev/null" {
				path = ""
			}
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			// Parse the new-side start independently to support omitted counts.
			i := strings.Index(line, " +")
			if i < 0 {
				inHunk = false
				continue
			}
			fields := strings.Fields(line[i+2:])
			if len(fields) == 0 {
				inHunk = false
				continue
			}
			part := fields[0]
			part = strings.SplitN(part, ",", 2)[0]
			parsedLine, err := strconv.Atoi(part)
			if err != nil {
				inHunk = false
				continue
			}
			newLine = parsedLine
			inHunk = path != ""
			continue
		}
		if !inHunk || line == "\\ No newline at end of file" || line == "" {
			continue
		}
		switch line[0] {
		case ' ':
			if result[path] == nil {
				result[path] = map[int]struct{}{}
			}
			result[path][newLine] = struct{}{}
			newLine++
		case '+':
			if result[path] == nil {
				result[path] = map[int]struct{}{}
			}
			result[path][newLine] = struct{}{}
			newLine++
		case '-':
		default:
			inHunk = false
		}
	}
	return result
}

func Validate(diff string, comments []Comment) []Comment {
	diffLines := DiffLines(diff)
	out := make([]Comment, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line < 1 || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if _, ok := diffLines[c.Path][c.Line]; ok {
			out = append(out, c)
		}
	}
	return out
}

func GiteaComments(diff string, comments []Comment) []gitea.InlineComment {
	diffLines := DiffLines(diff)
	out := make([]gitea.InlineComment, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line < 1 || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if _, ok := diffLines[c.Path][c.Line]; !ok {
			continue
		}
		out = append(out, gitea.InlineComment{Path: c.Path, Body: c.Body, Line: c.Line})
	}
	return out
}
