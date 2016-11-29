// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/pkg/errors"
)

var varGen = map[string]func(p *planner) string{
	`DATABASE`:                      func(p *planner) string { return p.session.Database },
	`DEFAULT_TRANSACTION_ISOLATION`: func(p *planner) string { return p.session.DefaultIsolationLevel.String() },
	`SYNTAX`:                        func(p *planner) string { return parser.Syntax(p.session.Syntax).String() },
	`TIME ZONE`:                     func(p *planner) string { return p.session.Location.String() },
	`TRANSACTION ISOLATION LEVEL`:   func(p *planner) string { return p.txn.Proto.Isolation.String() },
	`TRANSACTION PRIORITY`:          func(p *planner) string { return p.txn.UserPriority.String() },
	`MAX_INDEX_KEYS`:                func(_ *planner) string { return "32" },
	`SEARCH_PATH`:                   func(p *planner) string { return strings.Join(p.session.SearchPath, ", ") },
}
var varNames = func() []string {
	res := make([]string, 0, len(varGen))
	for vName := range varGen {
		res = append(res, vName)
	}
	sort.Strings(res)
	return res
}()

const (
	checkSchema = `SELECT SCHEMA_NAME FROM information_schema.schemata WHERE SCHEMA_NAME=$1 LIMIT 1`
	checkTable  = `SELECT TABLE_SCHEMA FROM information_schema.tables WHERE TABLE_SCHEMA=$1
					AND TABLE_NAME=$2 LIMIT 1`
	checkTablePrivilege = `SELECT TABLE_NAME FROM information_schema.table_privileges
							WHERE TABLE_SCHEMA=$1 AND TABLE_NAME=$2 AND GRANTEE=$3 LIMIT 1`
)

// Show a session-local variable name.
func (p *planner) Show(n *parser.Show) (planNode, error) {
	name := strings.ToUpper(n.Name)

	var columns ResultColumns

	switch name {
	case `ALL`:
		columns = ResultColumns{
			{Name: "Variable", Typ: parser.TypeString},
			{Name: "Value", Typ: parser.TypeString},
		}
	default:
		if _, ok := varGen[name]; !ok {
			return nil, fmt.Errorf("unknown variable: %q", name)
		}
		columns = ResultColumns{{Name: name, Typ: parser.TypeString}}
	}

	return &delayedNode{
		p:       p,
		name:    "SHOW " + name,
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			switch name {
			case `ALL`:
				for _, vName := range varNames {
					gen := varGen[vName]
					value := gen(p)
					if _, err := v.rows.AddRow(
						parser.DTuple{parser.NewDString(vName), parser.NewDString(value)},
					); err != nil {
						v.rows.Close()
						return nil, err
					}
				}
			default:
				// The key in varGen is guaranteed to exist thanks to the
				// check above.
				gen := varGen[name]
				value := gen(p)
				if _, err := v.rows.AddRow(parser.DTuple{parser.NewDString(value)}); err != nil {
					v.rows.Close()
					return nil, err
				}
			}

			return v, nil
		},
	}, nil
}

// ShowColumns of a table.
// Privileges: Any privilege on table.
//   Notes: postgres does not have a SHOW COLUMNS statement.
//          mysql only returns columns you have privileges on.
func (p *planner) ShowColumns(n *parser.ShowColumns) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	columns := ResultColumns{
		{Name: "Field", Typ: parser.TypeString},
		{Name: "Type", Typ: parser.TypeString},
		{Name: "Null", Typ: parser.TypeBool},
		{Name: "Default", Typ: parser.TypeString},
	}
	return &delayedNode{
		p:       p,
		name:    "SHOW COLUMNS FROM " + tn.String(),
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			const getColumns = `SELECT COLUMN_NAME AS "Field", DATA_TYPE AS "Type", (IS_NULLABLE!='NO') AS "Null",` +
				` COLUMN_DEFAULT AS "Default" FROM information_schema.columns WHERE TABLE_SCHEMA=$1 AND TABLE_NAME=$2` +
				` ORDER BY ORDINAL_POSITION`
			{
				// Check if the database exists by using the security.RootUser.
				values, err := p.queryRowsAsRoot(checkSchema, tn.Database())
				if err != nil {
					return nil, err
				}
				if len(values) == 0 {
					return nil, sqlbase.NewUndefinedDatabaseError(tn.Database())
				}
			}

			{
				// Check if the table exists by using the security.RootUser.
				values, err := p.queryRowsAsRoot(checkTable, tn.Database(), tn.Table())
				if err != nil {
					return nil, err
				}
				if len(values) == 0 {
					return nil, sqlbase.NewUndefinedTableError(tn.String())
				}
			}

			// Check if the user has been granted.
			// Skip the checking if the table is a virtual table.
			{
				virDesc, err := p.session.virtualSchemas.getVirtualTableDesc(tn)
				if err != nil {
					return nil, err
				}

				if virDesc == nil {
					values, err := p.QueryRow(checkTablePrivilege, tn.Database(), tn.Table(), p.session.User)
					if err != nil {
						return nil, err
					}
					if len(values) == 0 {
						return nil, fmt.Errorf("user %s has no privileges on table %s", p.session.User, tn.String())
					}
				}
			}
			// Temporarily set the current database to get visibility into
			// information_schema if the current user isn't root.
			origDatabase := p.evalCtx.Database
			p.evalCtx.Database = tn.Database()
			defer func() { p.evalCtx.Database = origDatabase }()

			// Get columns of table from information_schema.columns.
			rows, err := p.queryRows(getColumns, tn.Database(), tn.Table())
			if err != nil {
				return nil, err
			}
			v := p.newContainerValuesNode(columns, 0)
			for _, r := range rows {
				if _, err := v.rows.AddRow(r); err != nil {
					v.rows.Close()
					return nil, err
				}
			}
			return v, nil
		},
	}, nil
}

// showCreateInterleave returns an INTERLEAVE IN PARENT clause for the specified
// index, if applicable.
func (p *planner) showCreateInterleave(idx *sqlbase.IndexDescriptor) (string, error) {
	if len(idx.Interleave.Ancestors) == 0 {
		return "", nil
	}
	intl := idx.Interleave
	parentTable, err := sqlbase.GetTableDescFromID(p.txn, intl.Ancestors[len(intl.Ancestors)-1].TableID)
	if err != nil {
		return "", err
	}
	var sharedPrefixLen int
	for _, ancestor := range intl.Ancestors {
		sharedPrefixLen += int(ancestor.SharedPrefixLen)
	}
	interleavedColumnNames := quoteNames(idx.ColumnNames[:sharedPrefixLen]...)
	s := fmt.Sprintf(" INTERLEAVE IN PARENT %s (%s)", parentTable.Name, interleavedColumnNames)
	return s, nil
}

// ShowCreateTable returns a CREATE TABLE statement for the specified table in
// Traditional syntax.
// Privileges: Any privilege on table.
func (p *planner) ShowCreateTable(n *parser.ShowCreateTable) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	desc, err := p.mustGetTableDesc(tn)
	if err != nil {
		return nil, err
	}
	if err := p.anyPrivilege(desc); err != nil {
		return nil, err
	}

	columns := ResultColumns{
		{Name: "Table", Typ: parser.TypeString},
		{Name: "CreateTable", Typ: parser.TypeString},
	}

	return &delayedNode{
		p:       p,
		name:    "SHOW CREATE TABLE " + tn.String(),
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			var buf bytes.Buffer
			fmt.Fprintf(&buf, "CREATE TABLE %s (", quoteNames(n.Table.String()))
			var primary string
			for i, col := range desc.VisibleColumns() {
				if i != 0 {
					buf.WriteString(",")
				}
				buf.WriteString("\n\t")
				fmt.Fprintf(&buf, "%s %s", quoteNames(col.Name), col.Type.SQLString())
				if col.Nullable {
					buf.WriteString(" NULL")
				} else {
					buf.WriteString(" NOT NULL")
				}
				if col.DefaultExpr != nil {
					fmt.Fprintf(&buf, " DEFAULT %s", *col.DefaultExpr)
				}
				if desc.IsPhysicalTable() && desc.PrimaryIndex.ColumnIDs[0] == col.ID {
					// Only set primary if the primary key is on a visible column (not rowid).
					primary = fmt.Sprintf(",\n\tCONSTRAINT %s PRIMARY KEY (%s)",
						quoteNames(desc.PrimaryIndex.Name),
						quoteNames(desc.PrimaryIndex.ColumnNames...),
					)
				}
			}
			buf.WriteString(primary)
			for _, idx := range desc.Indexes {
				var storing string
				if len(idx.StoreColumnNames) > 0 {
					storing = fmt.Sprintf(" STORING (%s)", quoteNames(idx.StoreColumnNames...))
				}
				interleave, err := p.showCreateInterleave(&idx)
				if err != nil {
					v.rows.Close()
					return nil, err
				}
				fmt.Fprintf(&buf, ",\n\t%sINDEX %s (%s)%s%s",
					isUnique[idx.Unique],
					quoteNames(idx.Name),
					quoteNames(idx.ColumnNames...),
					storing,
					interleave,
				)
			}
			for _, fam := range desc.Families {
				activeColumnNames := make([]string, 0, len(fam.ColumnNames))
				for i, colID := range fam.ColumnIDs {
					if _, err := desc.FindActiveColumnByID(colID); err == nil {
						activeColumnNames = append(activeColumnNames, fam.ColumnNames[i])
					}
				}
				fmt.Fprintf(&buf, ",\n\tFAMILY %s (%s)",
					quoteNames(fam.Name),
					quoteNames(activeColumnNames...),
				)
			}

			for _, e := range desc.Checks {
				fmt.Fprintf(&buf, ",\n\t")
				if len(e.Name) > 0 {
					fmt.Fprintf(&buf, "CONSTRAINT %s ", quoteNames(e.Name))
				}
				fmt.Fprintf(&buf, "CHECK (%s)", e.Expr)
			}

			buf.WriteString("\n)")

			interleave, err := p.showCreateInterleave(&desc.PrimaryIndex)
			if err != nil {
				v.rows.Close()
				return nil, err
			}
			buf.WriteString(interleave)

			if _, err := v.rows.AddRow(parser.DTuple{
				parser.NewDString(n.Table.String()),
				parser.NewDString(buf.String()),
			}); err != nil {
				v.rows.Close()
				return nil, err
			}
			return v, nil
		},
	}, nil
}

var isUnique = map[bool]string{true: "UNIQUE "}

// quoteName quotes based on Traditional syntax and adds commas between names.
func quoteNames(names ...string) string {
	nameList := make(parser.NameList, len(names))
	for i, n := range names {
		nameList[i] = parser.Name(n)
	}
	return parser.AsString(nameList)
}

// ShowCreateView returns a CREATE VIEW statement for the specified view in
// Traditional syntax.
// Privileges: Any privilege on view.
func (p *planner) ShowCreateView(n *parser.ShowCreateView) (planNode, error) {
	tn, err := n.View.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	desc, err := p.mustGetViewDesc(tn)
	if err != nil {
		return nil, err
	}
	if err := p.anyPrivilege(desc); err != nil {
		return nil, err
	}

	columns := ResultColumns{
		{Name: "View", Typ: parser.TypeString},
		{Name: "CreateView", Typ: parser.TypeString},
	}
	return &delayedNode{
		p:       p,
		name:    "SHOW CREATE VIEW " + tn.String(),
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			var buf bytes.Buffer
			fmt.Fprintf(&buf, "CREATE VIEW %s ", quoteNames(n.View.String()))

			// Determine whether custom column names were specified when the view
			// was created, and include them if so.
			customColNames := false
			stmt, err := parser.ParseOneTraditional(desc.ViewQuery)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse underlying query from view %q", tn)
			}
			sel, ok := stmt.(*parser.Select)
			if !ok {
				return nil, errors.Errorf("failed to parse underlying query from view %q as a select", tn)
			}

			// When constructing the Select plan, make sure we don't require any
			// privileges on the underlying tables.
			p.skipSelectPrivilegeChecks = true
			defer func() { p.skipSelectPrivilegeChecks = false }()

			sourcePlan, err := p.Select(sel, []parser.Type{}, false)
			if err != nil {
				return nil, err
			}
			for i, col := range sourcePlan.Columns() {
				if col.Name != desc.Columns[i].Name {
					customColNames = true
					break
				}
			}
			if customColNames {
				colNames := make([]string, 0, len(desc.Columns))
				for _, col := range desc.Columns {
					colNames = append(colNames, col.Name)
				}
				fmt.Fprintf(&buf, "(%s) ", strings.Join(colNames, ", "))
			}

			fmt.Fprintf(&buf, "AS %s", desc.ViewQuery)
			if _, err := v.rows.AddRow(parser.DTuple{
				parser.NewDString(n.View.String()),
				parser.NewDString(buf.String()),
			}); err != nil {
				v.rows.Close()
				return nil, err
			}
			return v, nil
		},
	}, nil
}

// ShowDatabases returns all the databases.
// Privileges: None.
//   Notes: postgres does not have a "show databases"
//          mysql has a "SHOW DATABASES" permission, but we have no system-level permissions.
func (p *planner) ShowDatabases(n *parser.ShowDatabases) (planNode, error) {
	const getDatabases = `SELECT SCHEMA_NAME AS "Database" FROM information_schema.schemata
							ORDER BY "Database"`
	stmt, err := parser.ParseOneTraditional(getDatabases)
	if err != nil {
		return nil, err
	}
	return p.newPlan(stmt, nil, true)
}

// ShowGrants returns grant details for the specified objects and users.
// TODO(marc): implement no targets (meaning full scan).
// Privileges: None.
//   Notes: postgres does not have a SHOW GRANTS statement.
//          mysql only returns the user's privileges.
func (p *planner) ShowGrants(n *parser.ShowGrants) (planNode, error) {
	if n.Targets == nil {
		return nil, errors.Errorf("TODO(marc): implement SHOW GRANT with no targets")
	}

	objectType := "Database"
	if n.Targets.Tables != nil {
		objectType = "Table"
	}

	columns := ResultColumns{
		{Name: objectType, Typ: parser.TypeString},
		{Name: "User", Typ: parser.TypeString},
		{Name: "Privileges", Typ: parser.TypeString},
	}

	return &delayedNode{
		p:       p,
		name:    "SHOW GRANTS",
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)
			// Check if the target exists.
			checkFn := func(sql string, args ...interface{}) (bool, error) {
				values, err := p.queryRowsAsRoot(sql, args...)
				if err != nil {
					return false, err
				}

				if len(values) > 0 {
					return true, nil
				}
				return false, nil
			}

			queryFn := func(sql string, args ...interface{}) error {
				rows, err := p.queryRows(sql, args...)
				if err != nil {
					return err
				}
				for _, r := range rows {
					if _, err := v.rows.AddRow(r); err != nil {
						return err
					}
				}
				return nil
			}

			// Get grants of database from information_schema.schema_privileges
			// if the type of target is database.
			if n.Targets.Databases != nil {
				// TODO(nvanbenschoten): Clean up parameter assignment throughout.
				var params []interface{}
				var paramHolders []string
				paramSeq := 1
				for _, db := range n.Targets.Databases.ToStrings() {
					exists, err := checkFn(checkSchema, db)
					if err != nil {
						v.rows.Close()
						return nil, err
					}
					if !exists {
						v.rows.Close()
						return nil, sqlbase.NewUndefinedDatabaseError(db)
					}
					paramHolders = append(paramHolders, fmt.Sprintf("$%d", paramSeq))
					paramSeq++
					params = append(params, db)
				}
				schemaGrants := fmt.Sprintf(`SELECT TABLE_SCHEMA AS "Database", GRANTEE AS "User",
									PRIVILEGE_TYPE AS "Privileges" FROM information_schema.schema_privileges
									WHERE TABLE_SCHEMA IN (%s)`, strings.Join(paramHolders, ","))
				if n.Grantees != nil {
					paramHolders = paramHolders[:0]
					for _, grantee := range n.Grantees.ToStrings() {
						paramHolders = append(paramHolders, fmt.Sprintf("$%d", paramSeq))
						params = append(params, grantee)
						paramSeq++
					}
					schemaGrants = fmt.Sprintf(`%s AND GRANTEE IN(%s)`, schemaGrants, strings.Join(paramHolders, ","))
				}
				if err := queryFn(schemaGrants, params...); err != nil {
					v.rows.Close()
					return nil, err
				}
			}

			// Get grants of table from information_schema.table_privileges
			// if the type of target is table.
			if n.Targets.Tables != nil {
				// TODO(nvanbenschoten): Clean up parameter assignment throughout.
				var params []interface{}
				var paramHolders []string
				paramSeq := 1
				for _, tableTarget := range n.Targets.Tables {
					tableGlob, err := tableTarget.NormalizeTablePattern()
					if err != nil {
						v.rows.Close()
						return nil, err
					}
					tables, err := p.expandTableGlob(tableGlob)
					if err != nil {
						v.rows.Close()
						return nil, err
					}
					for i := range tables {
						exists, err := checkFn(checkTable, tables[i].Database(), tables[i].Table())
						if err != nil {
							v.rows.Close()
							return nil, err
						}
						if !exists {
							v.rows.Close()
							return nil, sqlbase.NewUndefinedTableError(tables[i].String())
						}
						paramHolders = append(paramHolders, fmt.Sprintf("($%d,$%d)",
							paramSeq, paramSeq+1))
						params = append(params, tables[i].Database(), tables[i].Table())
						paramSeq += 2

					}
				}
				tableGrants := fmt.Sprintf(`SELECT TABLE_NAME, GRANTEE, PRIVILEGE_TYPE FROM information_schema.table_privileges
									WHERE (TABLE_SCHEMA, TABLE_NAME) IN (%s)`, strings.Join(paramHolders, ","))
				if n.Grantees != nil {
					paramHolders = paramHolders[:0]
					for _, grantee := range n.Grantees.ToStrings() {
						paramHolders = append(paramHolders, fmt.Sprintf("$%d", paramSeq))
						params = append(params, grantee)
						paramSeq++
					}
					tableGrants = fmt.Sprintf(`%s AND GRANTEE IN(%s)`, tableGrants, strings.Join(paramHolders, ","))
				}
				if err := queryFn(tableGrants, params...); err != nil {
					v.rows.Close()
					return nil, err
				}
			}

			// Sort the result by target name, user name and privileges.
			sort := &sortNode{
				ctx: p.ctx(),
				p:   p,
				ordering: sqlbase.ColumnOrdering{
					{ColIdx: 0, Direction: encoding.Ascending},
					{ColIdx: 1, Direction: encoding.Ascending},
					{ColIdx: 2, Direction: encoding.Ascending},
				},
				columns: v.columns,
			}
			return &selectTopNode{source: v, sort: sort}, nil
		},
	}, nil
}

// ShowIndex returns all the indexes for a table.
// Privileges: Any privilege on table.
//   Notes: postgres does not have a SHOW INDEXES statement.
//          mysql requires some privilege for any column.
func (p *planner) ShowIndex(n *parser.ShowIndex) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	desc, err := p.mustGetTableDesc(tn)
	if err != nil {
		return nil, err
	}
	if err := p.anyPrivilege(desc); err != nil {
		return nil, err
	}

	columns := ResultColumns{
		{Name: "Table", Typ: parser.TypeString},
		{Name: "Name", Typ: parser.TypeString},
		{Name: "Unique", Typ: parser.TypeBool},
		{Name: "Seq", Typ: parser.TypeInt},
		{Name: "Column", Typ: parser.TypeString},
		{Name: "Direction", Typ: parser.TypeString},
		{Name: "Storing", Typ: parser.TypeBool},
	}

	return &delayedNode{
		p:       p,
		name:    "SHOW INDEX FROM " + tn.String(),
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			appendRow := func(index sqlbase.IndexDescriptor, colName string, sequence int,
				direction string, isStored bool) error {
				newRow := parser.DTuple{
					parser.NewDString(tn.Table()),
					parser.NewDString(index.Name),
					parser.MakeDBool(parser.DBool(index.Unique)),
					parser.NewDInt(parser.DInt(sequence)),
					parser.NewDString(colName),
					parser.NewDString(direction),
					parser.MakeDBool(parser.DBool(isStored)),
				}
				_, err := v.rows.AddRow(newRow)
				return err
			}

			for _, index := range append([]sqlbase.IndexDescriptor{desc.PrimaryIndex}, desc.Indexes...) {
				sequence := 1
				for i, col := range index.ColumnNames {
					if err := appendRow(index, col, sequence, index.ColumnDirections[i].String(), false); err != nil {
						v.rows.Close()
						return nil, err
					}
					sequence++
				}
				for _, col := range index.StoreColumnNames {
					if err := appendRow(index, col, sequence, "N/A", true); err != nil {
						v.rows.Close()
						return nil, err
					}
					sequence++
				}
			}
			return v, nil
		},
	}, nil
}

// ShowConstraints returns all the constraints for a table.
// Privileges: Any privilege on table.
//   Notes: postgres does not have a SHOW CONSTRAINTS statement.
//          mysql requires some privilege for any column.
func (p *planner) ShowConstraints(n *parser.ShowConstraints) (planNode, error) {
	tn, err := n.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	desc, err := p.mustGetTableDesc(tn)
	if err != nil {
		return nil, err
	}
	if err := p.anyPrivilege(desc); err != nil {
		return nil, err
	}

	columns := ResultColumns{
		{Name: "Table", Typ: parser.TypeString},
		{Name: "Name", Typ: parser.TypeString},
		{Name: "Type", Typ: parser.TypeString},
		{Name: "Column(s)", Typ: parser.TypeString},
		{Name: "Details", Typ: parser.TypeString},
	}

	return &delayedNode{
		p:       p,
		name:    "SHOW CONSTRAINTS FROM " + tn.String(),
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			info, err := desc.GetConstraintInfo(p.txn)
			if err != nil {
				return nil, err
			}
			for name, c := range info {
				detailsDatum := parser.DNull
				if c.Details != "" {
					detailsDatum = parser.NewDString(c.Details)
				}
				columnsDatum := parser.DNull
				if c.Columns != nil {
					columnsDatum = parser.NewDString(strings.Join(c.Columns, ", "))
				}
				kind := string(c.Kind)
				if c.Unvalidated {
					kind += " (UNVALIDATED)"
				}
				newRow := []parser.Datum{
					parser.NewDString(tn.Table()),
					parser.NewDString(name),
					parser.NewDString(kind),
					columnsDatum,
					detailsDatum,
				}
				if _, err := v.rows.AddRow(newRow); err != nil {
					v.Close()
					return nil, err
				}
			}

			// Sort the results by constraint name.
			sort := &sortNode{
				ctx: p.ctx(),
				p:   p,
				ordering: sqlbase.ColumnOrdering{
					{ColIdx: 0, Direction: encoding.Ascending},
					{ColIdx: 1, Direction: encoding.Ascending},
				},
				columns: v.columns,
			}
			return &selectTopNode{source: v, sort: sort}, nil
		},
	}, nil
}

// ShowTables returns all the tables.
// Privileges: None.
//   Notes: postgres does not have a SHOW TABLES statement.
//          mysql only returns tables you have privileges on.
func (p *planner) ShowTables(n *parser.ShowTables) (planNode, error) {
	name := p.session.Database
	if n.Database != "" {
		name = string(n.Database)
	}
	if name == "" {
		return nil, errNoDatabase
	}

	columns := ResultColumns{{Name: "Table", Typ: parser.TypeString}}
	return &delayedNode{
		p:       p,
		name:    "SHOW TABLES FROM " + name,
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			{
				// Check if the database exists by using the security.RootUser.
				values, err := p.queryRowsAsRoot(checkSchema, name)
				if err != nil {
					return nil, err
				}
				if len(values) == 0 {
					return nil, sqlbase.NewUndefinedDatabaseError(name)
				}
			}
			// Temporarily set the current database to get visibility into
			// information_schema if the current user isn't root.
			origDatabase := p.evalCtx.Database
			p.evalCtx.Database = name
			defer func() { p.evalCtx.Database = origDatabase }()

			// Get the tables of database from information_schema.tables.
			const getTables = `SELECT TABLE_NAME FROM information_schema.tables
							WHERE tables.TABLE_SCHEMA=$1 ORDER BY tables.TABLE_NAME`
			rows, err := p.queryRows(getTables, name)
			if err != nil {
				return nil, err
			}

			v := p.newContainerValuesNode(columns, 0)
			for _, r := range rows {
				if _, err := v.rows.AddRow(r); err != nil {
					v.rows.Close()
					return nil, err
				}
			}
			return v, nil
		},
	}, nil
}

// ShowUsers returns all the users.
// Privileges: SELECT on system.users.
func (p *planner) ShowUsers(n *parser.ShowUsers) (planNode, error) {
	stmt, err := parser.ParseOneTraditional(`SELECT username FROM system.users`)
	if err != nil {
		return nil, err
	}
	return p.newPlan(stmt, nil, true)
}

// Help returns usage information for the builtin functions
// Privileges: None
func (p *planner) Help(n *parser.Help) (planNode, error) {
	name := strings.ToLower(n.Name.String())
	columns := ResultColumns{
		{Name: "Function", Typ: parser.TypeString},
		{Name: "Signature", Typ: parser.TypeString},
		{Name: "Category", Typ: parser.TypeString},
		{Name: "Details", Typ: parser.TypeString},
	}
	return &delayedNode{
		p:       p,
		name:    "HELP " + name,
		columns: columns,
		constructor: func(p *planner) (planNode, error) {
			v := p.newContainerValuesNode(columns, 0)

			matches, ok := parser.Builtins[name]
			// TODO(dt): support fuzzy matching.
			if !ok {
				return v, nil
			}

			for _, f := range matches {
				row := parser.DTuple{
					parser.NewDString(name),
					parser.NewDString(f.Signature()),
					parser.NewDString(f.Category()),
					parser.NewDString(f.Info),
				}
				if _, err := v.rows.AddRow(row); err != nil {
					v.Close()
					return nil, err
				}
			}
			return v, nil
		},
	}, nil
}
