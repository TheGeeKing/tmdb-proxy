package main

import (
	"bufio"
	"errors"
	"os"
	"regexp"
	"strings"
)

// loadDotEnv accepts literal KEY=value lines; the process environment takes precedence.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read .env")
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	name := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if !ok || !name.MatchString(k) {
			return errors.New("invalid .env line; expected KEY=value")
		}
		if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, `'`) {
			if len(v) < 2 || v[len(v)-1] != v[0] {
				return errors.New("invalid .env quoted value")
			}
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			if os.Setenv(k, v) != nil {
				return errors.New("cannot load .env setting")
			}
		}
	}
	if scanner.Err() != nil {
		return errors.New("cannot read .env")
	}
	return nil
}
