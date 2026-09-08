package cli

import (
	"fmt"
	"strings"
)

// ParseKV splits "KEY=VALUE" arguments, as used by `oak secrets set`.
func ParseKV(args []string) (map[string]string, error) {
	m := make(map[string]string, len(args))
	for _, arg := range args {
		i := strings.IndexByte(arg, '=')
		if i <= 0 {
			return nil, fmt.Errorf("invalid KEY=VALUE pair %q", arg)
		}
		m[arg[:i]] = arg[i+1:]
	}
	return m, nil
}
