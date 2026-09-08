// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package config

// TextFilesConfig defines the interface to access a named set of inline text files.
//
// The document carries content only; what is done with it is decided by whatever references it, so
// far only a container's textFiles mount.
type TextFilesConfig interface {
	NamedDocument

	// FileContents returns the files keyed by a path relative to the set's root.
	//
	// Named FileContents rather than Files because the document's own field is called Files.
	FileContents() map[string]string
}
