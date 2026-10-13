package torrentzip_test

import (
	"bytes"
	"fmt"
	"log"

	"github.com/bodgit/torrentzip"
)

func ExampleWriter() {
	buf := new(bytes.Buffer)

	w := torrentzip.NewWriter(buf)

	files := []struct {
		Name, Body string
	}{
		{"readme.txt", "This archive contains some text files."},
		{"gopher.txt", "Gopher names:\nGeorge\nGeoffrey\nGonzo"},
		{"todo.txt", "Get animal handling licence.\nWrite more examples."},
	}
	for _, file := range files {
		f, err := w.Create(file.Name)
		if err != nil {
			log.Fatal(err)
		}

		if _, err := f.Write([]byte(file.Body)); err != nil {
			log.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	fmt.Println(buf.String()[buf.Len()-22:])
	// Output: TORRENTZIPPED-9F25FE2F
}
