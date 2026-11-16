package diffvalidate

import (
	"bufio"
	"strconv"
	"strings"
)

type Comment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

// Positions maps new-file line numbers to Gitea review new_position values.
// Gitea defines new_position as the line position within the unified diff, not
// the absolute file line number; context and added lines advance that position.
func Positions(diff string) map[string]map[int]int {
	result := map[string]map[int]int{}
	s := bufio.NewScanner(strings.NewReader(diff))
	s.Buffer(make([]byte, 4096), 4*1024*1024)
	path, newLine, pos, inHunk := "", 0, 0, false
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "diff --git ") {
			path = ""
			pos = 0
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
			part := strings.Fields(line[i+2:])[0]
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
			pos++
			if result[path] == nil {
				result[path] = map[int]int{}
			}
			result[path][newLine] = pos
			newLine++
		case '+':
			pos++
			if result[path] == nil {
				result[path] = map[int]int{}
			}
			result[path][newLine] = pos
			newLine++
		case '-':
			pos++
		default:
			inHunk = false
		}
	}
	return result
}

func Validate(diff string, comments []Comment) []Comment {
	positions := Positions(diff)
	out := make([]Comment, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line < 1 || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if _, ok := positions[c.Path][c.Line]; ok {
			out = append(out, c)
		}
	}
	return out
}

func GiteaComments(diff string, comments []Comment) []map[string]any {
	positions := Positions(diff)
	out := make([]map[string]any, 0, len(comments))
	for _, c := range comments {
		if c.Path == "" || c.Line < 1 || strings.TrimSpace(c.Body) == "" {
			continue
		}
		p, ok := positions[c.Path][c.Line]
		if !ok {
			continue
		}
		out = append(out, map[string]any{"path": c.Path, "body": c.Body, "new_position": p, "old_position": 0})
	}
	return out
}
