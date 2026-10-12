package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-n" {
		fmt.Print(strings.Join(os.Args[2:], " "))
	} else {
		fmt.Println(strings.Join(os.Args[1:], " "))
	}
}
