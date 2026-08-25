package main

import (
	"fmt"

	"whatskept/internal/workspace"
)

func runInit(dir string) error {
	already, err := workspace.Init(dir)
	if err != nil {
		return err
	}
	if already {
		fmt.Printf("%s is already a whatskept workspace\n", dir)
		return nil
	}
	fmt.Printf("initialized whatskept workspace in %s\n", dir)
	return nil
}
