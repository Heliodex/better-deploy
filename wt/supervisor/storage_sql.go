package main

import (
	"fmt"
	"strings"
)

// readStorageQuotedWord returns the contents of a double-quoted or backtick-quoted identifier. SQLite allows function names such as load_extension to be quoted, so these contents must be checked too.
func readStorageQuotedWord(query string, i int, quote byte) (string, int) {
	var word strings.Builder
	i++
	for i < len(query) {
		if query[i] != quote {
			word.WriteByte(query[i])
			i++
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			word.WriteByte(quote)
			i += 2
			continue
		}
		return word.String(), i + 1
	}
	return word.String(), i
}

func readStorageBracketWord(query string, i int) (string, int) {
	i++
	start := i
	for i < len(query) && query[i] != ']' {
		i++
	}
	word := query[start:i]
	if i < len(query) {
		i++
	}
	return word, i
}

func skipStorageString(query string, i int) int {
	i++
	for i < len(query) {
		if query[i] != '\'' {
			i++
			continue
		}
		if i+1 < len(query) && query[i+1] == '\'' {
			i += 2
			continue
		}
		return i + 1
	}
	return i
}

func skipStorageLineComment(query string, i int) int {
	i += 2
	for i < len(query) && query[i] != '\n' {
		i++
	}
	return i
}

func skipStorageBlockComment(query string, i int) int {
	i += 2
	for i+1 < len(query) {
		if query[i] == '*' && query[i+1] == '/' {
			return i + 2
		}
		i++
	}
	return len(query)
}

func isStorageWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func storageSQLWords(query string) []string {
	words := []string{}
	for i := 0; i < len(query); {
		switch c := query[i]; {
		case c == '\'':
			i = skipStorageString(query, i)
		case c == '"' || c == '`':
			word, next := readStorageQuotedWord(query, i, c)
			if word != "" {
				words = append(words, strings.ToLower(word))
			}
			i = next
		case c == '[':
			word, next := readStorageBracketWord(query, i)
			if word != "" {
				words = append(words, strings.ToLower(word))
			}
			i = next
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			i = skipStorageLineComment(query, i)
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			i = skipStorageBlockComment(query, i)
		case isStorageWordByte(c):
			start := i
			for i < len(query) && isStorageWordByte(query[i]) {
				i++
			}
			words = append(words, strings.ToLower(query[start:i]))
		default:
			i++
		}
	}
	return words
}

// validateStorageSQL rejects the few SQLite features that can reach outside the app's database file. ATTACH and VACUUM INTO are also blocked by SQLITE_LIMIT_ATTACHED; the word check keeps the error understandable and covers older SQLite builds.
func validateStorageSQL(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("sql must not be empty")
	}
	if len(query) > maxStorageSQL {
		return fmt.Errorf("sql is too long (max %d bytes)", maxStorageSQL)
	}
	words := storageSQLWords(query)
	if len(words) > 0 {
		switch words[0] {
		case "begin", "commit", "rollback", "end", "savepoint", "release":
			return fmt.Errorf("transaction control statements are not allowed; use /v1/batch")
		}
	}
	for _, word := range words {
		switch word {
		case "attach", "detach", "vacuum", "pragma", "load_extension", "readfile", "writefile", "writable_schema":
			return fmt.Errorf("sql uses %q, which is not allowed", word)
		}
	}
	return nil
}
