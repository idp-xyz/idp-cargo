// cargo-settings-doc writes the settings catalogue document, docs/settings/catalogue.md, from the catalogues of FIN and
// SEA and from the profiles (V2.102). Run it from the repository's root after a catalogue or a profile changes; a test
// fails when the committed document differs from what it writes.
package main

import (
	"flag"
	"fmt"
	"os"

	"go.idp.xyz/idp-cargo/internal/bootstrap"
)

func main() {
	out := flag.String("o", bootstrap.SettingsCatalogueDocPath, "file to write; - writes to standard output")
	flag.Parse()
	doc := bootstrap.SettingsCatalogueDocument()
	if *out == "-" {
		fmt.Print(doc)
		return
	}
	if err := os.WriteFile(*out, []byte(doc), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-settings-doc:", err)
		os.Exit(1)
	}
}
