//go:build !unix

package toft

import "os"

// ownedByMe has no owner to compare off Unix; the mode check is what remains.
func ownedByMe(os.FileInfo) bool { return true }
