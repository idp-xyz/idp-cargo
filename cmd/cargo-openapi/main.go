// cargo-openapi writes the OpenAPI 3.1 document of Cargo's HTTP API, made from the route table (since V2.106):
//
//	go run ./cmd/cargo-openapi > contracts/openapi/cargo.json
//
// A test holds contracts/openapi/cargo.json to the routes, so the published document is always the routes'.
package main

import (
	"fmt"
	"os"

	"go.idp.xyz/idp-cargo/internal/bootstrap"
)

func main() {
	doc, err := bootstrap.OpenAPIDocument()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cargo-openapi:", err)
		os.Exit(1)
	}
	if _, err = os.Stdout.Write(doc); err != nil {
		fmt.Fprintln(os.Stderr, "cargo-openapi:", err)
		os.Exit(1)
	}
}
