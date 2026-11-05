package native

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// globPatternAlternativeLimit bounds how many alternatives a brace pattern expands to.
const globPatternAlternativeLimit = 64

// compiledGlob is a file name or path pattern (*, **, ?, {a,b}); the transcript tools match
// with it.
type compiledGlob struct {
	byPath      bool
	expressions []*regexp.Regexp
}

func (glob *compiledGlob) matches(name, path string) bool {
	value := name
	if glob.byPath {
		value = path
	}
	for _, expression := range glob.expressions {
		if expression.MatchString(value) {
			return true
		}
	}
	return false
}

// expandGlobBraces expands the first brace alternative recursively, stopping at
// the same bounded number of alternatives as the TypeScript implementation.
func expandGlobBraces(pattern string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	relativeClose := strings.IndexByte(pattern[open+1:], '}')
	if relativeClose < 0 {
		return []string{pattern}
	}
	close := open + 1 + relativeClose
	options := strings.Split(pattern[open+1:close], ",")
	if len(options) < 2 {
		return []string{pattern}
	}
	results := make([]string, 0, limit)
	for _, option := range options {
		next := pattern[:open] + option + pattern[close+1:]
		for _, expanded := range expandGlobBraces(next, limit) {
			results = append(results, expanded)
			if len(results) >= limit {
				return results
			}
		}
	}
	return results
}

func globRegexpSource(pattern string) string {
	var source strings.Builder
	source.WriteByte('^')
	for index := 0; index < len(pattern); {
		character := pattern[index]
		switch character {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				if index+2 < len(pattern) && pattern[index+2] == '/' {
					// Go's regexp package has no non-capturing groups.
					source.WriteString("(.*/)?")
					index += 3
					continue
				}
				source.WriteString(".*")
				index += 2
				continue
			}
			source.WriteString("[^/]*")
			index++
		case '?':
			source.WriteString("[^/]")
			index++
		default:
			_, size := utf8.DecodeRuneInString(pattern[index:])
			source.WriteString(regexp.QuoteMeta(pattern[index : index+size]))
			index += size
		}
	}
	source.WriteByte('$')
	return source.String()
}

func compileGlob(pattern string) (*compiledGlob, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return nil, nil
	}
	compiled := &compiledGlob{byPath: strings.Contains(pattern, "/")}
	for _, alternative := range expandGlobBraces(pattern, globPatternAlternativeLimit) {
		expression, err := regexp.Compile(globRegexpSource(alternative))
		if err != nil {
			return nil, err
		}
		compiled.expressions = append(compiled.expressions, expression)
	}
	return compiled, nil
}
