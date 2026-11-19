package holocropper

import (
	"path/filepath"
)

// Path helpers shared by every pass. Scratch and output placement live in the
// request structs (AnalyzeRequest.TempDir, RenderRequest.OutputDir); the
// fused pipeline's own temp-dir juggling went away with it in story C3.

func convertToAbsolutePath(path string) (string, error) {
	// Check if the path is already absolute
	if filepath.IsAbs(path) {
		return path, nil
	}

	// Convert relative path to absolute path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	return absPath, nil
}

func getBasenameWithoutExt(absPath string) string {
	// Get the base name of the file
	base := filepath.Base(absPath)

	// Remove the file extension
	ext := filepath.Ext(base)
	basename := base[:len(base)-len(ext)]

	return basename
}
