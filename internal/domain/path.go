package domain

import (
	"golang.org/x/text/unicode/norm"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DefaultReservedDirectory = ".clouddrive-trash"

// ValidateName normalizes Unicode to NFC without changing case. It rejects
// traversal instead of silently cleaning it into a different target.
func ValidateName(name string, reserved ...string) (string, error) {
	if !utf8.ValidString(name) {
		return "", NewError(InvalidPath, "invalid UTF-8")
	}
	name = norm.NFC.String(name)
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, "/\\") {
		return "", NewError(InvalidPath, "invalid path component")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", NewError(InvalidPath, "control characters are forbidden")
		}
	}
	names := append([]string{DefaultReservedDirectory}, reserved...)
	for _, r := range names {
		if name == norm.NFC.String(strings.Trim(r, "/")) {
			return "", NewError(InvalidPath, "reserved namespace")
		}
	}
	return name, nil
}

// NormalizePath accepts absolute business paths only. This function receives a
// decoded application path, never a raw URL; callers must not decode it again.
func NormalizePath(value string, reserved ...string) (string, error) {
	if !utf8.ValidString(value) || !strings.HasPrefix(value, "/") || len(value) > 4096 {
		return "", NewError(InvalidPath, "an absolute UTF-8 path is required")
	}
	if value == "/" {
		return "/", nil
	}
	parts := strings.Split(value, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		v, err := ValidateName(part, reserved...)
		if err != nil {
			return "", err
		}
		clean = append(clean, v)
	}
	result := "/" + strings.Join(clean, "/")
	if len(result) > 4096 {
		return "", NewError(InvalidPath, "path is too long")
	}
	return result, nil
}

// ValidateMove rejects cycles and out-of-space moves before the official oCIS
// operation. The caller must lock the relevant subtree in the database as well.
func ValidateMove(file File, targetParent File, ancestorIDs []string) error {
	if file.OwnerID != targetParent.OwnerID || file.SpaceID != targetParent.SpaceID {
		return NewError(AccessDenied, "cross-space and ownership transfers are not supported")
	}
	if targetParent.Kind != KindDirectory || targetParent.Lifecycle != LifecycleActive {
		return NewError(InvalidPath, "target parent is not an active directory")
	}
	if file.ID == targetParent.ID {
		return NewError(InvalidPath, "a directory cannot contain itself")
	}
	for _, id := range ancestorIDs {
		if id == file.ID {
			return NewError(InvalidPath, "a directory cannot move into its own subtree")
		}
	}
	return nil
}

func IsDescendantPath(candidate, parent string) bool {
	if parent == "/" {
		return candidate != "/" && strings.HasPrefix(candidate, "/")
	}
	return strings.HasPrefix(candidate, path.Clean(parent)+"/")
}
