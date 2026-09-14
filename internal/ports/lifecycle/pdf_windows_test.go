//go:build windows

// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
package lifecycle

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gen2brain/go-fitz"
)

func TestNativeMuPDFTextImageAndErrorRecovery(t *testing.T) {
	// Exercise the bundled UCRT library, including its setjmp error path.
	for cycle := 0; cycle < 3; cycle++ {
		broken, err := fitz.NewFromMemory([]byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF\n"))
		if err == nil {
			broken.Close()
			t.Fatal("invalid PDF unexpectedly opened")
		}
		var pdf strings.Builder
		pdf.WriteString("%PDF-1.4\n")
		content := "BT /F1 12 Tf 20 50 Td (Semantic Windows) Tj ET\n"
		objects := []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 100] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
			"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
			fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		}
		offsets := []int{0}
		for i, object := range objects {
			offsets = append(offsets, pdf.Len())
			fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, object)
		}
		xref := pdf.Len()
		fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
		for _, offset := range offsets[1:] {
			fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
		}
		fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
		doc, err := fitz.NewFromMemory([]byte(pdf.String()))
		if err != nil {
			t.Fatal(err)
		}
		text, err := doc.Text(0)
		if err != nil || !strings.Contains(text, "Semantic Windows") {
			t.Fatalf("text: %q, %v", text, err)
		}
		page, err := doc.Image(0)
		if err != nil || page.Bounds().Empty() {
			t.Fatalf("render: %v", err)
		}
		if err := doc.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
