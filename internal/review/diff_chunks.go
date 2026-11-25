package review

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var unifiedHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

type patchLine struct {
	text string
	old  bool
	new  bool
}

func splitDiff(diff string, maxBytes int) ([]string, error) {
	if maxBytes < 1 {
		return nil, fmt.Errorf("PR metadata alone exceeds the review context safety limit")
	}
	if diff == "" {
		return nil, fmt.Errorf("PR metadata and review history exceed the review context safety limit without a diff to split")
	}
	lines := strings.SplitAfter(diff, "\n")
	var sections [][]string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diff --git ") || len(sections) == 0 {
			sections = append(sections, nil)
		}
		sections[len(sections)-1] = append(sections[len(sections)-1], line)
	}
	var units []string
	for _, section := range sections {
		sectionUnits, err := splitDiffSection(section, maxBytes)
		if err != nil {
			return nil, err
		}
		units = append(units, sectionUnits...)
	}
	var chunks []string
	var chunk strings.Builder
	for _, unit := range units {
		if chunk.Len() > 0 && chunk.Len()+len(unit) > maxBytes {
			chunks = append(chunks, chunk.String())
			chunk.Reset()
		}
		if len(unit) > maxBytes {
			return nil, fmt.Errorf("diff section is %d bytes and exceeds the available %d-byte context space", len(unit), maxBytes)
		}
		chunk.WriteString(unit)
	}
	if chunk.Len() > 0 {
		chunks = append(chunks, chunk.String())
	}
	return chunks, nil
}

func splitDiffSection(section []string, maxBytes int) ([]string, error) {
	prefixEnd := 0
	for prefixEnd < len(section) && !strings.HasPrefix(section[prefixEnd], "@@ ") {
		prefixEnd++
	}
	prefix := strings.Join(section[:prefixEnd], "")
	if prefixEnd == len(section) {
		if len(prefix) > maxBytes {
			return nil, fmt.Errorf("diff file header is %d bytes and exceeds the available %d-byte context space", len(prefix), maxBytes)
		}
		return []string{prefix}, nil
	}
	var units []string
	for i := prefixEnd; i < len(section); {
		header := section[i]
		match := unifiedHunkHeader.FindStringSubmatch(strings.TrimSuffix(header, "\n"))
		if match == nil {
			return nil, fmt.Errorf("cannot parse unified diff hunk header %q", strings.TrimSpace(header))
		}
		oldStart, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse old diff line number: %w", err)
		}
		newStart, err := strconv.Atoi(match[3])
		if err != nil {
			return nil, fmt.Errorf("parse new diff line number: %w", err)
		}
		i++
		body := make([]patchLine, 0)
		for i < len(section) && !strings.HasPrefix(section[i], "@@ ") {
			line := section[i]
			if strings.HasPrefix(line, "diff --git ") {
				break
			}
			if line == "" {
				body = append(body, patchLine{text: line})
			} else {
				switch line[0] {
				case ' ':
					body = append(body, patchLine{text: line, old: true, new: true})
				case '-':
					body = append(body, patchLine{text: line, old: true})
				case '+':
					body = append(body, patchLine{text: line, new: true})
				case '\\':
					body = append(body, patchLine{text: line})
				default:
					return nil, fmt.Errorf("unexpected line in unified diff hunk: %q", strings.TrimSpace(line))
				}
			}
			i++
		}
		pieces, err := splitHunk(prefix, oldStart, newStart, match[5], body, maxBytes)
		if err != nil {
			return nil, err
		}
		units = append(units, pieces...)
	}
	return units, nil
}

func splitHunk(prefix string, oldStart, newStart int, suffix string, body []patchLine, maxBytes int) ([]string, error) {
	var pieces []string
	oldLine, newLine := oldStart, newStart
	var current []patchLine
	currentOld, currentNew := oldLine, newLine
	oldCount, newCount := 0, 0
	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@%s\n", currentOld, oldCount, currentNew, newCount, suffix)
		var piece strings.Builder
		piece.WriteString(prefix)
		piece.WriteString(header)
		for _, line := range current {
			piece.WriteString(line.text)
		}
		if piece.Len() > maxBytes {
			return fmt.Errorf("a diff hunk line and its headers exceed the available %d-byte context space", maxBytes)
		}
		pieces = append(pieces, piece.String())
		current = nil
		oldCount, newCount = 0, 0
		return nil
	}
	for _, line := range body {
		if len(current) == 0 {
			currentOld, currentNew = oldLine, newLine
		}
		current = append(current, line)
		if line.old {
			oldCount++
			oldLine++
		}
		if line.new {
			newCount++
			newLine++
		}
		candidateSize := len(prefix) + len(fmt.Sprintf("@@ -%d,%d +%d,%d @@%s\n", currentOld, oldCount, currentNew, newCount, suffix))
		for _, item := range current {
			candidateSize += len(item.text)
		}
		if candidateSize <= maxBytes {
			continue
		}
		last := current[len(current)-1]
		current = current[:len(current)-1]
		if last.old {
			oldLine--
			oldCount--
		}
		if last.new {
			newLine--
			newCount--
		}
		if err := flush(); err != nil {
			return nil, err
		}
		currentOld, currentNew = oldLine, newLine
		current = append(current, last)
		if last.old {
			oldCount++
			oldLine++
		}
		if last.new {
			newCount++
			newLine++
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return pieces, nil
}
