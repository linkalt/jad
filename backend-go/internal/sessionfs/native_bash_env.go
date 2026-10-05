package sessionfs

import (
	"os"
	"regexp"
	"strings"
	"unicode"
)

var bashEnvAssignmentPattern = regexp.MustCompile(`^(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=(.*)$`)

func loadNativeBashEnvFile(path, settingPath string) (map[string]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, configError("%s: could not read %s: %v", settingPath, path, err)
	}
	return parseNativeBashEnvFile(string(contents), path)
}

func parseNativeBashEnvFile(contents, path string) (map[string]string, error) {
	values := make(map[string]string)
	contents = strings.TrimPrefix(contents, "\uFEFF")
	for index, rawLine := range strings.Split(contents, "\n") {
		lineNumber := index + 1
		line := strings.TrimLeft(rawLine, " \t\r\n\v\f")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		assignment := bashEnvAssignmentPattern.FindStringSubmatch(line)
		if len(assignment) != 3 {
			return nil, configError("%s:%d: expected NAME=value", path, lineNumber)
		}
		value, err := parseNativeBashEnvValue(assignment[2], path, lineNumber)
		if err != nil {
			return nil, err
		}
		values[assignment[1]] = value
	}
	return values, nil
}

func parseNativeBashEnvValue(value, path string, lineNumber int) (string, error) {
	trimmed := strings.TrimLeft(value, " \t\r\n\v\f")
	if trimmed == "" {
		return "", nil
	}
	quote := trimmed[0]
	if quote != '"' && quote != '\'' {
		for index, character := range trimmed {
			if character == '#' && index > 0 {
				previous := rune(trimmed[index-1])
				if unicode.IsSpace(previous) {
					trimmed = trimmed[:index]
					break
				}
			}
		}
		return strings.TrimRightFunc(trimmed, unicode.IsSpace), nil
	}

	closeIndex := -1
	escaped := false
	for index := 1; index < len(trimmed); index++ {
		character := trimmed[index]
		if quote == '"' && character == '\\' && !escaped {
			escaped = true
			continue
		}
		if character == quote && !escaped {
			closeIndex = index
			break
		}
		escaped = false
	}
	if closeIndex < 0 {
		return "", configError("%s:%d: unterminated quoted value", path, lineNumber)
	}
	suffix := strings.TrimLeft(trimmed[closeIndex+1:], " \t\r\n\v\f")
	if suffix != "" && !strings.HasPrefix(suffix, "#") {
		return "", configError("%s:%d: unexpected content after quoted value", path, lineNumber)
	}
	quoted := trimmed[1:closeIndex]
	if quote == '\'' {
		return quoted, nil
	}
	var output strings.Builder
	output.Grow(len(quoted))
	for index := 0; index < len(quoted); index++ {
		if quoted[index] != '\\' || index+1 >= len(quoted) {
			output.WriteByte(quoted[index])
			continue
		}
		next := quoted[index+1]
		switch next {
		case '\\', '"':
			output.WriteByte(next)
			index++
		case 'n':
			output.WriteByte('\n')
			index++
		case 'r':
			output.WriteByte('\r')
			index++
		case 't':
			output.WriteByte('\t')
			index++
		default:
			output.WriteByte('\\')
		}
	}
	return output.String(), nil
}
