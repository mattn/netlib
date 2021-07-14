// Copyright ©2016 The Gonum Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build ignore

// generate_blas creates a blas.go file from the provided C header file
// with optionally added documentation from the documentation package.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"modernc.org/cc"

	"gonum.org/v1/netlib/internal/binding"
)

const (
	header        = "cblas.h"
	srcModule     = "gonum.org/v1/gonum"
	documentation = "blas/gonum"
	target        = "blas.go"

	typ = "Implementation"

	prefix = "cblas_"

	warning = "Float32 implementations are autogenerated and not directly tested."
)

const (
	cribDocs      = true
	elideRepeat   = true
	noteOrigin    = true
	separateFuncs = false
)

var skip = map[string]bool{
	"cblas_errprn":    true,
	"cblas_srotg":     true,
	"cblas_srotmg":    true,
	"cblas_srotm":     true,
	"cblas_drotg":     true,
	"cblas_drotmg":    true,
	"cblas_drotm":     true,
	"cblas_crotg":     true,
	"cblas_zrotg":     true,
	"cblas_cdotu_sub": true,
	"cblas_cdotc_sub": true,
	"cblas_zdotu_sub": true,
	"cblas_zdotc_sub": true,

	// ATLAS extensions.
	"cblas_csrot": true,
	"cblas_zdrot": true,
}

var cToGoType = map[string]string{
	"int":    "int",
	"float":  "float32",
	"double": "float64",
}

var blasEnums = map[string]*template.Template{
	"CBLAS_ORDER":     template.Must(template.New("order").Parse("order")),
	"CBLAS_DIAG":      template.Must(template.New("diag").Parse("blas.Diag")),
	"CBLAS_TRANSPOSE": template.Must(template.New("trans").Parse("blas.Transpose")),
	"CBLAS_UPLO":      template.Must(template.New("uplo").Parse("blas.Uplo")),
	"CBLAS_SIDE":      template.Must(template.New("side").Parse("blas.Side")),
}

var cgoEnums = map[string]*template.Template{
	"CBLAS_ORDER":     template.Must(template.New("order").Parse("C.enum_CBLAS_ORDER(rowMajor)")),
	"CBLAS_DIAG":      template.Must(template.New("diag").Parse("C.enum_CBLAS_DIAG({{.}})")),
	"CBLAS_TRANSPOSE": template.Must(template.New("trans").Parse("C.enum_CBLAS_TRANSPOSE({{.}})")),
	"CBLAS_UPLO":      template.Must(template.New("uplo").Parse("C.enum_CBLAS_UPLO({{.}})")),
	"CBLAS_SIDE":      template.Must(template.New("side").Parse("C.enum_CBLAS_SIDE({{.}})")),
}

var cgoTypes = map[binding.TypeKey]*template.Template{
	{Kind: cc.Float, IsPointer: true}: template.Must(template.New("float*").Parse(
		`(*C.float)({{if eq . "alpha" "beta"}}&{{else}}_{{end}}{{.}})`,
	)),
	{Kind: cc.Double, IsPointer: true}: template.Must(template.New("double*").Parse(
		`(*C.double)({{if eq . "alpha" "beta"}}&{{else}}_{{end}}{{.}})`,
	)),
	{Kind: cc.Void, IsPointer: true}: template.Must(template.New("void*").Parse(
		`unsafe.Pointer({{if eq . "alpha" "beta"}}&{{else}}_{{end}}{{.}})`,
	)),
}

var (
	complex64Type = map[binding.TypeKey]*template.Template{
		{Kind: cc.Void, IsPointer: true}: template.Must(template.New("void*").Parse(
			`{{if eq . "alpha" "beta"}}complex64{{else}}[]complex64{{end}}`,
		))}

	complex128Type = map[binding.TypeKey]*template.Template{
		{Kind: cc.Void, IsPointer: true}: template.Must(template.New("void*").Parse(
			`{{if eq . "alpha" "beta"}}complex128{{else}}[]complex128{{end}}`,
		))}
)

var names = map[string]string{
	"uplo":   "ul",
	"trans":  "t",
	"transA": "tA",
	"transB": "tB",
	"side":   "s",
	"diag":   "d",
}

func shorten(n string) string {
	s, ok := names[n]
	if ok {
		return s
	}
	return n
}

func main() {
	decls, err := binding.Declarations(header)
	if err != nil {
		log.Fatal(err)
	}
	var docs map[string]map[string][]*ast.Comment
	if cribDocs {
		docs, err = binding.DocComments(pathTo(srcModule, documentation))
		if err != nil {
			log.Fatal(err)
		}
	}

	var buf bytes.Buffer

	h, err := template.New("handwritten").Parse(handwritten)
	if err != nil {
		log.Fatal(err)
	}
	err = h.Execute(&buf, header)
	if err != nil {
		log.Fatal(err)
	}

	var n int
	for _, d := range decls {
		if !strings.HasPrefix(d.Name, prefix) || skip[d.Name] {
			continue
		}
		if n != 0 && (separateFuncs || cribDocs) {
			buf.WriteByte('\n')
		}
		n++
		goSignature(&buf, d, docs[typ])
		if noteOrigin {
			fmt.Fprintf(&buf, "\t// declared at %s %s %s ...\n\n", d.Position(), d.Return, d.Name)
		}
		parameterChecks(&buf, d, parameterCheckRules)
		buf.WriteByte('\t')
		cgoCall(&buf, d)
		buf.WriteString("}\n")
	}

	b, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatal(err)
	}
	err = ioutil.WriteFile(target, b, 0664)
	if err != nil {
		log.Fatal(err)
	}
}

func goSignature(buf *bytes.Buffer, d binding.Declaration, docs map[string][]*ast.Comment) {
	blasName := strings.TrimPrefix(d.Name, prefix)
	goName := binding.UpperCaseFirst(blasName)

	if docs != nil {
		if doc, ok := docs[goName]; ok {
			if strings.Contains(doc[len(doc)-1].Text, warning) {
				doc = doc[:len(doc)-2]
			}
			for _, c := range doc {
				buf.WriteString(c.Text)
				buf.WriteByte('\n')
			}
		}
	}

	parameters := d.Parameters()

	var voidPtrType map[binding.TypeKey]*template.Template
	for _, p := range parameters {
		if p.Kind() == cc.Ptr && p.Elem().Kind() == cc.Void {
			switch {
			case blasName[0] == 'c', blasName[1] == 'c' && blasName[0] != 'z':
				voidPtrType = complex64Type
			case blasName[0] == 'z', blasName[1] == 'z':
				voidPtrType = complex128Type
			}
			break
		}
	}

	fmt.Fprintf(buf, "func (%s) %s(", typ, goName)
	c := 0
	for i, p := range parameters {
		if p.Kind() == cc.Enum && binding.GoTypeForEnum(p.Type(), "", blasEnums) == "order" {
			continue
		}
		if c != 0 {
			buf.WriteString(", ")
		}
		c++

		n := shorten(binding.LowerCaseFirst(p.Name()))
		var this, next string

		if p.Kind() == cc.Enum {
			this = binding.GoTypeForEnum(p.Type(), n, blasEnums)
		} else {
			this = binding.GoTypeFor(p.Type(), n, voidPtrType)
		}

		if elideRepeat && i < len(parameters)-1 && p.Type().Kind() == parameters[i+1].Type().Kind() {
			p := parameters[i+1]
			n := shorten(binding.LowerCaseFirst(p.Name()))
			if p.Kind() == cc.Enum {
				next = binding.GoTypeForEnum(p.Type(), n, blasEnums)
			} else {
				next = binding.GoTypeFor(p.Type(), n, voidPtrType)
			}
		}
		if next == this {
			buf.WriteString(n)
		} else {
			fmt.Fprintf(buf, "%s %s", n, this)
		}
	}
	if d.Return.Kind() != cc.Void {
		fmt.Fprintf(buf, ") %s {\n", cToGoType[d.Return.String()])
	} else {
		buf.WriteString(") {\n")
	}
}

func parameterChecks(buf *bytes.Buffer, d binding.Declaration, rules []func(*bytes.Buffer, binding.Declaration, binding.Parameter)) {
	for _, r := range rules {
		for _, p := range d.Parameters() {
			r(buf, d, p)
		}
	}
}

func cgoCall(buf *bytes.Buffer, d binding.Declaration) {
	if d.Return.Kind() != cc.Void {
		fmt.Fprintf(buf, "return %s(", cToGoType[d.Return.String()])
	}
	fmt.Fprintf(buf, "C.%s(", d.Name)
	for i, p := range d.Parameters() {
		if i != 0 {
			buf.WriteString(", ")
		}
		if p.Type().Kind() == cc.Enum {
			buf.WriteString(binding.CgoConversionForEnum(shorten(binding.LowerCaseFirst(p.Name())), p.Type(), cgoEnums))
		} else {
			buf.WriteString(binding.CgoConversionFor(shorten(binding.LowerCaseFirst(p.Name())), p.Type(), cgoTypes))
		}
	}
	if d.Return.Kind() != cc.Void {
		buf.WriteString(")")
	}
	buf.WriteString(")\n")
}

var parameterCheckRules = []func(*bytes.Buffer, binding.Declaration, binding.Parameter){
	trans,
	uplo,
	diag,
	side,
	shape,
	leadingDim,
	zeroInc,
	noWork,
	sliceLength,
	address,
}

func trans(buf *bytes.Buffer, d binding.Declaration, p binding.Parameter) {
	switch n := shorten(binding.LowerCaseFirst(p.Name())); n {
	case "t", "tA", "tB":
		switch {
		case strings.HasPrefix(d.Name, "cblas_ch"), strings.HasPrefix(d.Name, "cblas_zh"):
			fmt.Fprintf(buf, `	switch %[1]s {
	case blas.NoTrans:
		%[1]s = C.CblasNoTrans
	case blas.ConjTrans:
		%[1]s = C.CblasConjTrans
	default:
		panic(badTranspose)
	}
`, n)
		case strings.HasPrefix(d.Name, "cblas_cs"), strings.HasPrefix(d.Name, "cblas_zs"):
			fmt.Fprintf(buf, `	switch %[1]s {
	case blas.NoTrans:
		%[1]s = C.CblasNoTrans
	case blas.Trans:
		%[1]s = C.CblasTrans
	default:
		panic(badTranspose)
	}
`, n)
		default:
			fmt.Fprintf(buf, `	switch %[1]s {
	case blas.NoTrans:
		%[1]s = C.CblasNoTrans
	case blas.Trans:
		%[1]s = C.CblasTrans
	case blas.ConjTrans:
		%[1]s = C.CblasConjTrans
	default:
		panic(badTranspose)
	}
`, n)
		}
	}
}

func uplo(buf *bytes.Buffer, _ binding.Declaration, p binding.Parameter) {
	if p.Name() != "Uplo" {
		return
	}
	fmt.Fprint(buf, `	switch ul {
	case blas.Upper:
		ul = C.CblasUpper
	case blas.Lower:
		ul = C.CblasLower
	default:
		panic(badUplo)
	}
`)
}

func diag(buf *bytes.Buffer, _ binding.Declaration, p binding.Parameter) {
	if p.Name() != "Diag" {
		return
	}
	fmt.Fprint(buf, `	switch d {
	case blas.NonUnit:
		d = C.CblasNonUnit
	case blas.Unit:
		d = C.CblasUnit
	default:
		panic(badDiag)
	}
`)
	return
}

func side(buf *bytes.Buffer, _ binding.Declaration, p binding.Parameter) {
	if p.Name() != "Side" {
		return
	}
	fmt.Fprint(buf, `	switch s {
	case blas.Left:
		s = C.CblasLeft
	case blas.Right:
		s = C.CblasRight
	default:
		panic(badSide)
	}
`)
}

func shape(buf *bytes.Buffer, _ binding.Declaration, p binding.Parameter) {
	switch n := binding.LowerCaseFirst(p.Name()); n {
	case "m", "n", "k", "kL", "kU":
		fmt.Fprintf(buf, `	if %[1]s < 0 {
		panic(%[1]sLT0)
	}
`, n)
	}
}

func leadingDim(buf *bytes.Buffer, d binding.Declaration, p binding.Parameter) {
	pname := binding.LowerCaseFirst(p.Name())
	if !strings.HasPrefix(pname, "ld") {
		return
	}

	if pname == "ldc" {
		// C matrix has always n columns.
		fmt.Fprintf(buf, `	if ldc < max(1, n) {
		panic(badLdC)
	}
`)
		return
	}

	has := make(map[string]bool)
	for _, p := range d.Parameters() {
		has[shorten(binding.LowerCaseFirst(p.Name()))] = true
	}

	switch d.Name {
	case "cblas_sgemm", "cblas_dgemm", "cblas_cgemm", "cblas_zgemm":
		if pname == "lda" {
			fmt.Fprint(buf, `	var rowA, colA, rowB, colB int
	if tA == C.CblasNoTrans {
		rowA, colA = m, k
	} else {
		rowA, colA = k, m
	}
	if tB == C.CblasNoTrans {
		rowB, colB = k, n
	} else {
		rowB, colB = n, k
	}
	if lda < max(1, colA) {
		panic(badLdA)
	}
`)
		} else {
			fmt.Fprint(buf, `	if ldb < max(1, colB) {
		panic(badLdB)
	}
`)
		}
		return

	case "cblas_ssyrk", "cblas_dsyrk", "cblas_csyrk", "cblas_zsyrk",
		"cblas_ssyr2k", "cblas_dsyr2k", "cblas_csyr2k", "cblas_zsyr2k",
		"cblas_cherk", "cblas_zherk", "cblas_cher2k", "cblas_zher2k":
		if pname == "lda" {
			fmt.Fprint(buf, `	var row, col int
	if t == C.CblasNoTrans {
		row, col = n, k
	} else {
		row, col = k, n
	}
`)
		}
		fmt.Fprintf(buf, `	if %s < max(1, col) {
		panic(bad%s)
	}
`, pname, ldToPanicString(pname))
		return

	case "cblas_sgbmv", "cblas_dgbmv", "cblas_cgbmv", "cblas_zgbmv":
		fmt.Fprintf(buf, `	if lda < kL+kU+1 {
		panic(badLdA)
	}
`)
		return
	}

	switch {
	case has["k"]:
		// cblas_stbmv cblas_dtbmv cblas_ctbmv cblas_ztbmv
		// cblas_stbsv cblas_dtbsv cblas_ctbsv cblas_ztbsv
		// cblas_ssbmv cblas_dsbmv cblas_chbmv cblas_zhbmv
		fmt.Fprintf(buf, `	if lda < k+1 {
		panic(badLdA)
	}
`)
	case has["s"] && pname == "lda":
		// cblas_ssymm cblas_dsymm cblas_csymm cblas_zsymm
		// cblas_strmm cblas_dtrmm cblas_ctrmm cblas_ztrmm
		// cblas_strsm cblas_dtrsm cblas_ctrsm cblas_ztrsm
		// cblas_chemm cblas_zhemm
		fmt.Fprintf(buf, `	var k int
	if s == C.CblasLeft {
		k = m
	} else {
		k = n
	}
	if lda < max(1, k) {
		panic(badLdA)
	}
`)
	default:
		fmt.Fprintf(buf, `	if %s < max(1, n) {
		panic(bad%s)
	}
`, pname, ldToPanicString(pname))
	}
}

func zeroInc(buf *bytes.Buffer, _ binding.Declaration, p binding.Parameter) {
	switch n := binding.LowerCaseFirst(p.Name()); n {
	case "incX":
		fmt.Fprintf(buf, `	if incX == 0 {
		panic(zeroIncX)
	}
`)
	case "incY":
		fmt.Fprintf(buf, `	if incY == 0 {
		panic(zeroIncY)
	}
`)
	}
	return
}

func noWork(buf *bytes.Buffer, d binding.Declaration, p binding.Parameter) {
	if d.CParameters[len(d.CParameters)-1] != p.Parameter {
		return // Come back later.
	}

	switch d.Name {
	case "cblas_snrm2", "cblas_dnrm2", "cblas_scnrm2", "cblas_dznrm2",
		"cblas_sasum", "cblas_dasum", "cblas_scasum", "cblas_dzasum":
		fmt.Fprint(buf, `
	// Quick return if possible.
	if n == 0 || incX < 0 {
		return 0
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
`)
		return

	case "cblas_sscal", "cblas_dscal", "cblas_cscal", "cblas_zscal", "cblas_csscal", "cblas_zdscal":
		fmt.Fprint(buf, `
	// Quick return if possible.
	if n == 0 || incX < 0 {
		return
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
`)
		return

	case "cblas_isamax", "cblas_idamax", "cblas_icamax", "cblas_izamax":
		fmt.Fprint(buf, `
	// Quick return if possible.
	if n == 0 || incX < 0 {
		return -1
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
`)
		return
	}

	var value string
	switch d.Return.String() {
	case "float", "double":
		value = " 0"
	}
	var hasM bool
	for _, p := range d.Parameters() {
		if shorten(binding.LowerCaseFirst(p.Name())) == "m" {
			hasM = true
		}
	}
	if !hasM {
		fmt.Fprintf(buf, `
	// Quick return if possible.
	if n == 0 {
		return%s
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
`, value)
	} else {
		fmt.Fprintf(buf, `
	// Quick return if possible.
	if m == 0 || n == 0 {
		return
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
`)
	}
}

func sliceLength(buf *bytes.Buffer, d binding.Declaration, p binding.Parameter) {
	pname := shorten(binding.LowerCaseFirst(p.Name()))
	switch pname {
	case "a", "b", "c", "ap", "x", "y":
	default:
		return
	}

	if pname == "ap" {
		fmt.Fprint(buf, `	if len(ap) < n*(n+1)/2 {
		panic(shortAP)
	}
`)
		return
	}

	has := make(map[string]bool)
	for _, p := range d.Parameters() {
		has[shorten(binding.LowerCaseFirst(p.Name()))] = true
	}

	if pname == "c" {
		if p.Type().Kind() != cc.Ptr {
			// srot or drot
			return
		}
		if has["m"] {
			fmt.Fprint(buf, `	if len(c) < ldc*(m-1)+n {
		panic(shortC)
	}
`)
			return
		}
		fmt.Fprint(buf, `	if len(c) < ldc*(n-1)+n {
		panic(shortC)
	}
`)
		return
	}

	switch d.Name {
	case "cblas_snrm2", "cblas_dnrm2", "cblas_scnrm2", "cblas_dznrm2",
		"cblas_sasum", "cblas_dasum", "cblas_scasum", "cblas_dzasum",
		"cblas_sscal", "cblas_dscal", "cblas_cscal", "cblas_zscal", "cblas_csscal", "cblas_zdscal",
		"cblas_isamax", "cblas_idamax", "cblas_icamax", "cblas_izamax":
		fmt.Fprint(buf, `	if len(x) <= (n-1)*incX {
		panic(shortX)
	}
`)
		return

	case "cblas_ssyrk", "cblas_dsyrk", "cblas_csyrk", "cblas_zsyrk",
		"cblas_ssyr2k", "cblas_dsyr2k", "cblas_csyr2k", "cblas_zsyr2k",
		"cblas_cherk", "cblas_zherk", "cblas_cher2k", "cblas_zher2k":
		switch pname {
		case "a":
			// row and col have already been declared in leadingDim.
			fmt.Fprintf(buf, `	if len(a) < lda*(row-1)+col {
		panic(shortA)
	}
`)
		case "b":
			fmt.Fprintf(buf, `	if len(b) < ldb*(row-1)+col {
		panic(shortB)
	}
`)
		}
		return

	case "cblas_sgemm", "cblas_dgemm", "cblas_cgemm", "cblas_zgemm":
		switch pname {
		case "a":
			// rowA and colA have already been declared in leadingDim.
			fmt.Fprint(buf, `	if len(a) < lda*(rowA-1)+colA {
		panic(shortA)
	}
`)
		case "b":
			fmt.Fprint(buf, `	if len(b) < ldb*(rowB-1)+colB {
		panic(shortB)
	}
`)
		}
		return

	case "cblas_sgbmv", "cblas_dgbmv", "cblas_cgbmv", "cblas_zgbmv",
		"cblas_sgemv", "cblas_dgemv", "cblas_cgemv", "cblas_zgemv":
		switch pname {
		case "x":
			fmt.Fprint(buf, `	var lenX, lenY int
	if tA == C.CblasNoTrans {
		lenX, lenY = n, m
	} else {
		lenX, lenY = m, n
	}
	if (incX > 0 && len(x) <= (lenX-1)*incX) || (incX < 0 && len(x) <= (1-lenX)*incX) {
		panic(shortX)
	}
`)
		case "y":
			fmt.Fprint(buf, `	if (incY > 0 && len(y) <= (lenY-1)*incY) || (incY < 0 && len(y) <= (1-lenY)*incY) {
		panic(shortY)
	}
`)
		case "a":
			if has["kL"] {
				fmt.Fprintf(buf, `	if len(a) < lda*(min(m, n+kL)-1)+kL+kU+1 {
		panic(shortA)
	}
`)
			} else {
				fmt.Fprint(buf, `	if len(a) < lda*(m-1)+n {
		panic(shortA)
	}
`)
			}
		}
		return
	}

	switch pname {
	case "x":
		var label string
		if has["m"] {
			label = "m"
		} else {
			label = "n"
		}
		fmt.Fprintf(buf, `	if (incX > 0 && len(x) <= (%[1]s-1)*incX) || (incX < 0 && len(x) <= (1-%[1]s)*incX) {
		panic(shortX)
	}
`, label)

	case "y":
		fmt.Fprint(buf, `	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
`)

	case "a":
		switch {
		case has["s"]:
			fmt.Fprintf(buf, `	if len(a) < lda*(k-1)+k {
		panic(shortA)
	}
`)
		case has["k"]:
			fmt.Fprintf(buf, `	if len(a) < lda*(n-1)+k+1 {
		panic(shortA)
	}
`)
		case has["m"]:
			fmt.Fprint(buf, `	if len(a) < lda*(m-1)+n {
		panic(shortA)
	}
`)
		default:
			fmt.Fprint(buf, `	if len(a) < lda*(n-1)+n {
		panic(shortA)
	}
`)
		}

	case "b":
		fmt.Fprint(buf, `	if len(b) < ldb*(m-1)+n {
		panic(shortB)
	}
`)
	}

	return
}

var addrTypes = map[string]string{
	"char":   "byte",
	"int":    "int32",
	"float":  "float32",
	"double": "float64",
}

func address(buf *bytes.Buffer, d binding.Declaration, p binding.Parameter) {
	n := shorten(binding.LowerCaseFirst(p.Name()))
	blasName := strings.TrimPrefix(d.Name, prefix)
	switch n {
	case "a", "b", "c", "ap", "x", "y":
	default:
		return
	}
	if p.Type().Kind() == cc.Ptr {
		t := addrTypes[strings.TrimPrefix(p.Type().Element().String(), "const ")]
		if t == "" {
			switch {
			case blasName[0] == 'c', blasName[1] == 'c' && blasName[0] != 'z':
				t = "complex64"
			case blasName[0] == 'z', blasName[1] == 'z':
				t = "complex128"
			}
		}
		fmt.Fprintf(buf, `	var _%[1]s *%[2]s
	if len(%[1]s) > 0 {
		_%[1]s = &%[1]s[0]
	}
`, n, t)
	}
	return
}

func ldToPanicString(ld string) string {
	switch ld {
	case "lda":
		return "LdA"
	case "ldb":
		return "LdB"
	case "ldc":
		return "LdC"
	default:
		panic("unexpected ld")
	}
}

// pathTo returns the path to package within the given module. If running
// in module mode, this will look within the module in $GOPATH/pkg/mod
// at the correct version, otherwise it will find the version installed
// at $GOPATH/src/module/pkg.
func pathTo(module, pkg string) string {
	gopath, ok := os.LookupEnv("GOPATH")
	if !ok {
		var err error
		gopath, err = os.UserHomeDir()
		if err != nil {
			log.Fatal(err)
		}
		gopath = filepath.Join(gopath, "go")
	}

	cmd := exec.Command("go", "list", "-m", module)
	var buf, stderr bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		log.Fatalf("module aware go list failed with stderr output %q: %v", stderr.String(), err)
	}
	version := strings.TrimSpace(strings.Join(strings.Split(buf.String(), " "), "@"))
	return filepath.Join(gopath, "pkg", "mod", version, pkg)
}

const handwritten = `// Code generated by "go generate gonum.org/v1/netlib/blas/netlib" from {{.}}; DO NOT EDIT.

// Copyright ©2014 The Gonum Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netlib

/*
#cgo CFLAGS: -g -O2
#cgo windows LDFLAGS: -lcblas
#include "{{.}}"
*/
import "C"

import (
	"unsafe"

	"gonum.org/v1/gonum/blas"
)

// Type check assertions:
var (
	_ blas.Float32    = Implementation{}
	_ blas.Float64    = Implementation{}
	_ blas.Complex64  = Implementation{}
	_ blas.Complex128 = Implementation{}
)

// Type order is used to specify the matrix storage format. We still interact with
// an API that allows client calls to specify order, so this is here to document that fact.
type order int

const rowMajor order = C.CblasRowMajor

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type Implementation struct{}

// Special cases...

type srotmParams struct {
	flag float32
	h    [4]float32
}

type drotmParams struct {
	flag float64
	h    [4]float64
}

func (Implementation) Srotg(a float32, b float32) (c float32, s float32, r float32, z float32) {
	C.cblas_srotg((*C.float)(&a), (*C.float)(&b), (*C.float)(&c), (*C.float)(&s))
	return c, s, a, b
}
func (Implementation) Srotmg(d1 float32, d2 float32, b1 float32, b2 float32) (p blas.SrotmParams, rd1 float32, rd2 float32, rb1 float32) {
	var pi srotmParams
	C.cblas_srotmg((*C.float)(&d1), (*C.float)(&d2), (*C.float)(&b1), C.float(b2), (*C.float)(unsafe.Pointer(&pi)))
	return blas.SrotmParams{Flag: blas.Flag(pi.flag), H: pi.h}, d1, d2, b1
}
func (Implementation) Srotm(n int, x []float32, incX int, y []float32, incY int, p blas.SrotmParams) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}
	if p.Flag < blas.Identity || p.Flag > blas.Diagonal {
		panic(badFlag)
	}

	// Quick return if possible.
	if n == 0 {
		return
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *float32
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *float32
	if len(y) > 0 {
		_y = &y[0]
	}
	pi := srotmParams{
		flag: float32(p.Flag),
		h:    p.H,
	}
	C.cblas_srotm(C.int(n), (*C.float)(_x), C.int(incX), (*C.float)(_y), C.int(incY), (*C.float)(unsafe.Pointer(&pi)))
}
func (Implementation) Drotg(a float64, b float64) (c float64, s float64, r float64, z float64) {
	C.cblas_drotg((*C.double)(&a), (*C.double)(&b), (*C.double)(&c), (*C.double)(&s))
	return c, s, a, b
}
func (Implementation) Drotmg(d1 float64, d2 float64, b1 float64, b2 float64) (p blas.DrotmParams, rd1 float64, rd2 float64, rb1 float64) {
	var pi drotmParams
	C.cblas_drotmg((*C.double)(&d1), (*C.double)(&d2), (*C.double)(&b1), C.double(b2), (*C.double)(unsafe.Pointer(&pi)))
	return blas.DrotmParams{Flag: blas.Flag(pi.flag), H: pi.h}, d1, d2, b1
}
func (Implementation) Drotm(n int, x []float64, incX int, y []float64, incY int, p blas.DrotmParams) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}
	if p.Flag < blas.Identity || p.Flag > blas.Diagonal {
		panic(badFlag)
	}

	// Quick return if possible.
	if n == 0 {
		return
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *float64
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *float64
	if len(y) > 0 {
		_y = &y[0]
	}
	pi := drotmParams{
		flag: float64(p.Flag),
		h:    p.H,
	}
	C.cblas_drotm(C.int(n), (*C.double)(_x), C.int(incX), (*C.double)(_y), C.int(incY), (*C.double)(unsafe.Pointer(&pi)))
}
func (Implementation) Cdotu(n int, x []complex64, incX int, y []complex64, incY int) (dotu complex64) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}

	// Quick return if possible.
	if n == 0 {
		return 0
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *complex64
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *complex64
	if len(y) > 0 {
		_y = &y[0]
	}
	C.cblas_cdotu_sub(C.int(n), unsafe.Pointer(_x), C.int(incX), unsafe.Pointer(_y), C.int(incY), unsafe.Pointer(&dotu))
	return dotu
}
func (Implementation) Cdotc(n int, x []complex64, incX int, y []complex64, incY int) (dotc complex64) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}

	// Quick return if possible.
	if n == 0 {
		return 0
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *complex64
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *complex64
	if len(y) > 0 {
		_y = &y[0]
	}
	C.cblas_cdotc_sub(C.int(n), unsafe.Pointer(_x), C.int(incX), unsafe.Pointer(_y), C.int(incY), unsafe.Pointer(&dotc))
	return dotc
}
func (Implementation) Zdotu(n int, x []complex128, incX int, y []complex128, incY int) (dotu complex128) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}

	// Quick return if possible.
	if n == 0 {
		return 0
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *complex128
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *complex128
	if len(y) > 0 {
		_y = &y[0]
	}
	C.cblas_zdotu_sub(C.int(n), unsafe.Pointer(_x), C.int(incX), unsafe.Pointer(_y), C.int(incY), unsafe.Pointer(&dotu))
	return dotu
}
func (Implementation) Zdotc(n int, x []complex128, incX int, y []complex128, incY int) (dotc complex128) {
	if n < 0 {
		panic(nLT0)
	}
	if incX == 0 {
		panic(zeroIncX)
	}
	if incY == 0 {
		panic(zeroIncY)
	}

	// Quick return if possible.
	if n == 0 {
		return 0
	}

	// For zero matrix size the following slice length checks are trivially satisfied.
	if (incX > 0 && len(x) <= (n-1)*incX) || (incX < 0 && len(x) <= (1-n)*incX) {
		panic(shortX)
	}
	if (incY > 0 && len(y) <= (n-1)*incY) || (incY < 0 && len(y) <= (1-n)*incY) {
		panic(shortY)
	}
	var _x *complex128
	if len(x) > 0 {
		_x = &x[0]
	}
	var _y *complex128
	if len(y) > 0 {
		_y = &y[0]
	}
	C.cblas_zdotc_sub(C.int(n), unsafe.Pointer(_x), C.int(incX), unsafe.Pointer(_y), C.int(incY), unsafe.Pointer(&dotc))
	return dotc
}

// Generated cases ...

`
