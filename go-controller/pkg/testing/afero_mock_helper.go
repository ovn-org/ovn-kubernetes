package testing

import "os"

type AferoFileMockHelper struct {
	FileName    string
	Permissions os.FileMode
	Content     []byte
}

type AferoDirMockHelper struct {
	DirName     string
	Permissions os.FileMode
	Files       []AferoFileMockHelper
}
