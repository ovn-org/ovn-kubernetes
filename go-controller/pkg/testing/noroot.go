package testing

import "os"

func NoRoot() bool {
	return os.Getenv("NOROOT") == "TRUE"
}
