package main

import "strings"

func withArgumentValue(args []string, value string, names ...string) []string {
	result := make([]string, 0, len(args)+2)
	for index := 0; index < len(args); index++ {
		matched := false
		for _, name := range names {
			if args[index] == name {
				matched = true
				if index+1 < len(args) {
					index++
				}
				break
			}
		}
		if !matched {
			result = append(result, args[index])
		}
	}
	value = strings.TrimSpace(value)
	if value != "" {
		result = append(result, names[0], value)
	}
	return result
}

func withConfigValue(args []string, key, value string) []string {
	result := make([]string, 0, len(args)+2)
	for index := 0; index < len(args); index++ {
		if (args[index] == "-c" || args[index] == "--config") && index+1 < len(args) {
			if _, ok := strings.CutPrefix(args[index+1], key+"="); ok {
				index++
				continue
			}
		}
		result = append(result, args[index])
	}
	value = strings.TrimSpace(value)
	if value != "" {
		result = append(result, "-c", key+"="+value)
	}
	return result
}
