package main

import (
	"log"

	"github.com/gonejack/webarchive-to-singlefile/cmd"
)

func main() {
	var c cmd.WarcToHtml
	if e := c.Run(); e != nil {
		log.Fatal(e)
	}
}
