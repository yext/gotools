// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/memoize"
	"golang.org/x/tools/internal/span"
)

type parseKey struct {
	file source.FileIdentity
	mode source.ParseMode
}

type parseGoHandle struct {
	handle *memoize.Handle
	file   source.FileHandle
	mode   source.ParseMode
}

type parseGoData struct {
	memoize.NoCopy
	ast *ast.File
	err error
}

// We use a counting semaphore to limit
// the number of parallel I/O calls per process.
var ioLimit = make(chan bool, 20)

func (c *cache) ParseGo(fh source.FileHandle, mode source.ParseMode) source.ParseGoHandle {
	key := parseKey{
		file: fh.Identity(),
		mode: mode,
	}
	h := c.store.Bind(key, func(ctx context.Context) interface{} {
		data := &parseGoData{}
		data.ast, data.err = parseGo(ctx, c, fh, mode)
		return data
	})
	return &parseGoHandle{
		handle: h,
	}
}

func (h *parseGoHandle) File() source.FileHandle {
	return h.file
}

func (h *parseGoHandle) Mode() source.ParseMode {
	return h.mode
}

func (h *parseGoHandle) Parse(ctx context.Context) (*ast.File, error) {
	v := h.handle.Get(ctx)
	if v == nil {
		return nil, ctx.Err()
	}
	data := v.(*parseGoData)
	return data.ast, data.err
}

func parseGo(ctx context.Context, c *cache, fh source.FileHandle, mode source.ParseMode) (*ast.File, error) {
	buf, _, err := fh.Read(ctx)
	if err != nil {
		return nil, err
	}
	parserMode := parser.AllErrors | parser.ParseComments
	if mode == source.ParseHeader {
		parserMode = parser.ImportsOnly
	}
	ast, err := parser.ParseFile(c.fset, fh.Identity().URI.Filename(), buf, parserMode)
	if err != nil {
		return ast, err
	}
	if mode == source.ParseExported {
		trimAST(ast)
	}
	//TODO: move the ast fixup code into here
	return ast, nil
}

// parseFiles reads and parses the Go source files and returns the ASTs
// of the ones that could be at least partially parsed, along with a list
// parse errors encountered, and a fatal error that prevented parsing.
//
// Because files are scanned in parallel, the token.Pos
// positions of the resulting ast.Files are not ordered.
//
func (imp *importer) parseFiles(filenames []string, ignoreFuncBodies bool) ([]*astFile, []error, error) {
	var (
		wg     sync.WaitGroup
		n      = len(filenames)
		parsed = make([]*astFile, n)
		errors = make([]error, n)
	)
	// TODO: change this function to return the handles
	// TODO: eliminate the wait group at this layer, it should be done in the parser
	for i, filename := range filenames {
		if err := imp.ctx.Err(); err != nil {
			return nil, nil, err
		}
		// get a file handle
		fh := imp.view.session.GetFile(span.FileURI(filename))
		// now get a parser
		mode := source.ParseFull
		if ignoreFuncBodies {
			mode = source.ParseExported
		}
		ph := imp.view.session.cache.ParseGo(fh, mode)
		// now read and parse in parallel
		wg.Add(1)
		go func(i int, filename string) {
			ioLimit <- true // wait
			defer func() {
				<-ioLimit // signal done
				wg.Done()
			}()
			// ParseFile may return a partial AST and an error.
			f, err := ph.Parse(imp.ctx)
			parsed[i], errors[i] = &astFile{
				file:      f,
				err:       err,
				isTrimmed: ignoreFuncBodies,
			}, err
			// TODO: move fixup into the parse function
			// Fix any badly parsed parts of the AST.
			if f != nil {
				tok := imp.fset.File(f.Pos())
				src, _, err := fh.Read(imp.ctx)
				if err == nil {
					imp.view.fix(imp.ctx, f, tok, src)
				}
			}
		}(i, filename)
	}
	wg.Wait()

	// Eliminate nils, preserving order.
	var o int
	for _, f := range parsed {
		if f != nil {
			parsed[o] = f
			o++
		}
	}
	parsed = parsed[:o]

	o = 0
	for _, err := range errors {
		if err != nil {
			errors[o] = err
			o++
		}
	}
	errors = errors[:o]

	return parsed, errors, nil
}

// sameFile returns true if x and y have the same basename and denote
// the same file.
//
func sameFile(x, y string) bool {
	if x == y {
		// It could be the case that y doesn't exist.
		// For instance, it may be an overlay file that
		// hasn't been written to disk. To handle that case
		// let x == y through. (We added the exact absolute path
		// string to the CompiledGoFiles list, so the unwritten
		// overlay case implies x==y.)
		return true
	}
	if strings.EqualFold(filepath.Base(x), filepath.Base(y)) { // (optimisation)
		if xi, err := os.Stat(x); err == nil {
			if yi, err := os.Stat(y); err == nil {
				return os.SameFile(xi, yi)
			}
		}
	}
	return false
}

// trimAST clears any part of the AST not relevant to type checking
// expressions at pos.
func trimAST(file *ast.File) {
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncDecl:
			n.Body = nil
		case *ast.BlockStmt:
			n.List = nil
		case *ast.CaseClause:
			n.Body = nil
		case *ast.CommClause:
			n.Body = nil
		case *ast.CompositeLit:
			// Leave elts in place for [...]T
			// array literals, because they can
			// affect the expression's type.
			if !isEllipsisArray(n.Type) {
				n.Elts = nil
			}
		}
		return true
	})
}

func isEllipsisArray(n ast.Expr) bool {
	at, ok := n.(*ast.ArrayType)
	if !ok {
		return false
	}
	_, ok = at.Len.(*ast.Ellipsis)
	return ok
}

// fix inspects and potentially modifies any *ast.BadStmts or *ast.BadExprs in the AST.
// We attempt to modify the AST such that we can type-check it more effectively.
func (v *view) fix(ctx context.Context, file *ast.File, tok *token.File, src []byte) {
	var parent ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		switch n := n.(type) {
		case *ast.BadStmt:
			if err := v.parseDeferOrGoStmt(n, parent, tok, src); err != nil {
				v.Session().Logger().Debugf(ctx, "unable to parse defer or go from *ast.BadStmt: %v", err)
			}
			return false
		default:
			parent = n
			return true
		}
	})
}

// parseDeferOrGoStmt tries to parse an *ast.BadStmt into a defer or a go statement.
//
// go/parser packages a statement of the form "defer x." as an *ast.BadStmt because
// it does not include a call expression. This means that go/types skips type-checking
// this statement entirely, and we can't use the type information when completing.
// Here, we try to generate a fake *ast.DeferStmt or *ast.GoStmt to put into the AST,
// instead of the *ast.BadStmt.
func (v *view) parseDeferOrGoStmt(bad *ast.BadStmt, parent ast.Node, tok *token.File, src []byte) error {
	// Check if we have a bad statement containing either a "go" or "defer".
	s := &scanner.Scanner{}
	s.Init(tok, src, nil, 0)

	var pos token.Pos
	var tkn token.Token
	var lit string
	for {
		if tkn == token.EOF {
			return fmt.Errorf("reached the end of the file")
		}
		if pos >= bad.From {
			break
		}
		pos, tkn, lit = s.Scan()
	}
	var stmt ast.Stmt
	switch lit {
	case "defer":
		stmt = &ast.DeferStmt{
			Defer: pos,
		}
	case "go":
		stmt = &ast.GoStmt{
			Go: pos,
		}
	default:
		return fmt.Errorf("no defer or go statement found")
	}

	// The expression after the "defer" or "go" starts at this position.
	from, _, _ := s.Scan()
	var to, curr token.Pos
FindTo:
	for {
		curr, tkn, lit = s.Scan()
		// TODO(rstambler): This still needs more handling to work correctly.
		// We encounter a specific issue with code that looks like this:
		//
		//      defer x.<>
		//      y := 1
		//
		// In this scenario, we parse it as "defer x.y", which then fails to
		// type-check, and we don't get completions as expected.
		switch tkn {
		case token.COMMENT, token.EOF, token.SEMICOLON, token.DEFINE:
			break FindTo
		}
		// to is the end of expression that should become the Fun part of the call.
		to = curr
	}
	if !from.IsValid() || tok.Offset(from) >= len(src) {
		return fmt.Errorf("invalid from position")
	}
	if !to.IsValid() || tok.Offset(to)+1 >= len(src) {
		return fmt.Errorf("invalid to position")
	}
	exprstr := string(src[tok.Offset(from) : tok.Offset(to)+1])
	expr, err := parser.ParseExpr(exprstr)
	if expr == nil {
		return fmt.Errorf("no expr in %s: %v", exprstr, err)
	}
	// parser.ParseExpr returns undefined positions.
	// Adjust them for the current file.
	v.offsetPositions(expr, from-1)

	// Package the expression into a fake *ast.CallExpr and re-insert into the function.
	call := &ast.CallExpr{
		Fun:    expr,
		Lparen: to,
		Rparen: to,
	}
	switch stmt := stmt.(type) {
	case *ast.DeferStmt:
		stmt.Call = call
	case *ast.GoStmt:
		stmt.Call = call
	}
	switch parent := parent.(type) {
	case *ast.BlockStmt:
		for i, s := range parent.List {
			if s == bad {
				parent.List[i] = stmt
				break
			}
		}
	}
	return nil
}

// offsetPositions applies an offset to the positions in an ast.Node.
// TODO(rstambler): Add more cases here as they become necessary.
func (v *view) offsetPositions(expr ast.Expr, offset token.Pos) {
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			n.NamePos += offset
			return false
		default:
			return true
		}
	})
}
