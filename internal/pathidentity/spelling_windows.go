//go:build windows

package pathidentity

func diskSpelling(path string) (string, error) {
	return path, nil
}
