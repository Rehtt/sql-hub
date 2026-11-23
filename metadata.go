package main

import (
	"maps"
	"slices"
	"strings"

	"github.com/go-mysql-org/go-mysql/mysql"
)

// Until USE selects a target, the gateway exposes its own database names.
func (s *session) queryBeforeSelection(query []byte) error {
	statement := strings.ToUpper(strings.Join(strings.Fields(strings.TrimSuffix(string(query), ";")), " "))
	var fields []string
	var rows [][]any
	switch statement {
	case "SHOW DATABASES", "SHOW SCHEMAS":
		fields = []string{"Database"}
		for _, alias := range slices.Sorted(maps.Keys(s.gateway.routes)) {
			rows = append(rows, []any{alias})
		}

	default:
		// mycli refreshes metadata before a database is selected. Treat every
		// information_schema query as empty until a mapping is selected. This
		// also handles differences in how client drivers render NULL parameters.
		if strings.Contains(statement, "INFORMATION_SCHEMA") {
			switch {
			case strings.Contains(statement, "REFERENCED_TABLE_NAME"):
				fields = []string{"TABLE_NAME", "COLUMN_NAME", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME"}
			case strings.Contains(statement, "COLUMN_TYPE"):
				fields = []string{"TABLE_NAME", "COLUMN_NAME", "COLUMN_TYPE"}
			case strings.Contains(statement, "TABLE_NAME") && strings.Contains(statement, "COLUMN_NAME"):
				fields = []string{"TABLE_NAME", "COLUMN_NAME"}
			default:
				fields = []string{"ROUTINE_NAME"}
			}
		} else if statement == "SHOW TABLES" || statement == "SHOW FULL TABLES" {
			fields = []string{"Tables_in_database"}
		} else {
			return s.front.WriteValue(s.configureBeforeSelection(query))
		}
	}
	result, err := mysql.BuildSimpleTextResultset(fields, rows)
	if err != nil {
		return s.front.WriteValue(err)
	}
	return s.front.WriteValue(&mysql.Result{Resultset: result})
}
