//go:build windows

package main

import "akitafrp/internal/ui"

func main() {
	if err := ui.Run("client"); err != nil {
		panic(err)
	}
}
