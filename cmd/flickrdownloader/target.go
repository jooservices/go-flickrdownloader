package main

import "fmt"

// requireURL reports an error when the shared -u flag was omitted.
func requireURL() error {
	if urlFlag == "" {
		return fmt.Errorf("provide -u <flickr-url>")
	}
	return nil
}
