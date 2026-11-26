package main

import (
	"github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	_ "github.com/pingcap/tidb/pkg/parser/test_driver"
)

// readOnlyStatementAllowed accepts statements that can be sent through a
// read-only mapping. Unknown or unparsable statements are rejected so a new
// SQL feature cannot accidentally bypass the gateway policy.
func readOnlyStatementAllowed(query []byte) bool {
	statements, _, err := parser.New().ParseSQL(string(query))
	if err != nil || len(statements) == 0 {
		return false
	}
	for _, statement := range statements {
		if !readOnlyStatementNodeAllowed(statement) {
			return false
		}
	}
	return true
}

func readOnlyStatementNodeAllowed(statement ast.StmtNode) bool {
	switch node := statement.(type) {
	case *ast.SelectStmt:
		if node.LockInfo != nil && node.LockInfo.LockType != ast.SelectLockNone {
			return false
		}
		return node.SelectIntoOpt == nil || node.SelectIntoOpt.Tp == ast.SelectIntoVars
	case *ast.SetStmt:
		for _, assignment := range node.Variables {
			if assignment.IsGlobal || assignment.IsInstance {
				return false
			}
		}
		return true
	case *ast.SetPwdStmt:
		return false
	case *ast.ExplainStmt:
		return node.Stmt == nil || readOnlyStatementNodeAllowed(node.Stmt)
	}

	switch ast.GetStmtLabel(statement) {
	case "Select", "Show", "DescTable", "ExplainSQL", "ExplainAnalyzeSQL",
		"Set", "Use", "Begin", "Commit", "Rollback", "Savepoint":
		return true
	default:
		return false
	}
}
