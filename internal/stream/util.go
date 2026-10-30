package stream

import "strings"

func split(s, sep string) []string { return strings.Split(s, sep) }
func trim(s string) string         { return strings.TrimSpace(s) }
