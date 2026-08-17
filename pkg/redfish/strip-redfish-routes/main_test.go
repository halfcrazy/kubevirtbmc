package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func parseMethod(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "api_service.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f.Decls[0].(*ast.FuncDecl)
}

func TestIsNotImplementedStub(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"generated stub", `package redfish

func (s *APIService) F() (server.Response, error) {
	return server.Response(http.StatusNotImplemented, nil), errors.New("F method not implemented")
}`, true},
		{"other package constant", `package redfish

func (s *APIService) F() (server.Response, error) {
	return server.Response(foo.StatusNotImplemented, nil), nil
}`, false},
		{"no reference", `package redfish

func (s *APIService) F() (server.Response, error) {
	return server.Response(http.StatusOK, nil), nil
}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotImplementedStub(parseMethod(t, tc.src)); got != tc.want {
				t.Errorf("isNotImplementedStub = %v, want %v", got, tc.want)
			}
		})
	}
}
