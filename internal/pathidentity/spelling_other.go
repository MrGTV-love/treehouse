//go:build !darwin && !linux && !windows

package pathidentity

func diskSpelling(path string) (string, error) {
	return enumeratedSpelling(path)
}
