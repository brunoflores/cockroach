// Copyright 2018 The Cockroach Authors.
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

package testcat

import (
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/coltypes"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/util"
)

type indexType int

const (
	primaryIndex indexType = iota
	uniqueIndex
	nonUniqueIndex
)

// CreateTable creates a test table from a parsed DDL statement and adds it to
// the catalog. This is intended for testing, and is not a complete (and
// probably not fully correct) implementation. It just has to be "good enough".
func (tc *Catalog) CreateTable(stmt *tree.CreateTable) *Table {
	tn, err := stmt.Table.Normalize()
	if err != nil {
		panic(fmt.Errorf("%s", err))
	}

	// Update the table name to include catalog and schema if not provided.
	tc.qualifyTableName(tn)

	tab := &Table{Name: *tn}
	// Add the columns.
	for _, def := range stmt.Defs {
		switch def := def.(type) {
		case *tree.ColumnTableDef:
			tab.addColumn(def)
		}
	}

	// Add the primary index (if there is one defined).
	for _, def := range stmt.Defs {
		switch def := def.(type) {
		case *tree.ColumnTableDef:
			if def.PrimaryKey {
				// Add the primary index over the single column.
				tab.addPrimaryColumnIndex(string(def.Name))
			}

		case *tree.UniqueConstraintTableDef:
			if def.PrimaryKey {
				tab.addIndex(&def.IndexTableDef, primaryIndex)
			}
		}
	}

	// If there is no primary index, add the hidden rowid column.
	if len(tab.Indexes) == 0 {
		rowid := &Column{Name: "rowid", Type: types.Int, Hidden: true}
		tab.Columns = append(tab.Columns, rowid)
		tab.addPrimaryColumnIndex(rowid.Name)
	}

	// Search for other relevant definitions.
	for _, def := range stmt.Defs {
		switch def := def.(type) {
		case *tree.UniqueConstraintTableDef:
			if !def.PrimaryKey {
				tab.addIndex(&def.IndexTableDef, uniqueIndex)
			}

		case *tree.IndexTableDef:
			tab.addIndex(def, nonUniqueIndex)
		}
		// TODO(rytaft): In the future we will likely want to check for unique
		// constraints, indexes, and foreign key constraints to determine
		// nullability, uniqueness, etc.
	}

	// Add the new table to the catalog.
	tc.AddTable(tab)

	return tab
}

// qualifyTableName updates the given table name to include catalog and schema
// if not already included.
func (tc *Catalog) qualifyTableName(name *tree.TableName) {
	if name.ExplicitSchema {
		if name.ExplicitCatalog {
			// Already 3 parts: nothing to do.
			return
		}

		if name.SchemaName == tree.PublicSchemaName {
			// Use the current database.
			name.CatalogName = testDB
			return
		}

		// Compatibility with CockroachDB v1.1: use D.public.T.
		name.CatalogName = name.SchemaName
		name.SchemaName = tree.PublicSchemaName
		name.ExplicitCatalog = true
		return
	}

	// Use the current database.
	name.CatalogName = testDB
	name.SchemaName = tree.PublicSchemaName
}

func (tt *Table) addColumn(def *tree.ColumnTableDef) {
	nullable := !def.PrimaryKey && def.Nullable.Nullability != tree.NotNull
	typ := coltypes.CastTargetToDatumType(def.Type)
	col := &Column{Name: string(def.Name), Type: typ, Nullable: nullable}
	tt.Columns = append(tt.Columns, col)
}

func (tt *Table) addIndex(def *tree.IndexTableDef, typ indexType) {
	idx := &Index{Name: tt.makeIndexName(def.Name, typ)}

	// Add explicit columns and mark key columns as not null.
	for _, colDef := range def.Columns {
		col := idx.addColumn(tt, string(colDef.Column), colDef.Direction, true /* makeUnique */)

		if typ == primaryIndex {
			col.Nullable = false
		}
	}

	if typ == primaryIndex {
		var pkOrdinals util.FastIntSet
		for _, c := range idx.Columns {
			pkOrdinals.Add(c.Ordinal)
		}
		// Add the rest of the columns in the table.
		for i := range tt.Columns {
			if !pkOrdinals.Contains(i) {
				idx.addColumnByOrdinal(tt, i, tree.Ascending, false /* makeUnique */)
			}
		}
		if len(tt.Indexes) != 0 {
			panic("primary index should always be 0th index")
		}
		tt.Indexes = append(tt.Indexes, idx)
		return
	}

	// Add implicit key columns from primary index.
	pkCols := tt.Indexes[opt.PrimaryIndex].Columns[:tt.Indexes[opt.PrimaryIndex].Unique]
	for _, pkCol := range pkCols {
		// Only add columns that aren't already part of index.
		found := false
		for _, colDef := range def.Columns {
			if pkCol.Column.ColName() == opt.ColumnName(colDef.Column) {
				found = true
			}
		}

		if !found {
			// Implicit column is only part of the index's set of unique columns
			// if the index *was not* declared as unique in the first place. The
			// implicit columns are added to make the index unique (as well as
			// to "cover" the primary index for lookups).
			name := string(pkCol.Column.ColName())
			makeUnique := typ != uniqueIndex
			idx.addColumn(tt, name, tree.Ascending, makeUnique)
		}
	}

	// Add storing columns.
	for _, name := range def.Storing {
		// Only add storing columns that weren't added as part of adding implicit
		// key columns.
		found := false
		for _, pkCol := range pkCols {
			if opt.ColumnName(name) == pkCol.Column.ColName() {
				found = true
			}
		}
		if !found {
			idx.addColumn(tt, string(name), tree.Ascending, false /* makeUnique */)
		}
	}

	tt.Indexes = append(tt.Indexes, idx)
}

func (tt *Table) makeIndexName(defName tree.Name, typ indexType) string {
	name := string(defName)
	if name == "" {
		if typ == primaryIndex {
			name = "primary"
		} else {
			name = "secondary"
		}
	}
	return name
}

func (ti *Index) addColumn(
	tt *Table, name string, direction tree.Direction, makeUnique bool,
) *Column {
	return ti.addColumnByOrdinal(tt, tt.FindOrdinal(name), direction, makeUnique)
}

func (ti *Index) addColumnByOrdinal(
	tt *Table, ord int, direction tree.Direction, makeUnique bool,
) *Column {
	col := tt.Column(ord)
	idxCol := opt.IndexColumn{
		Column:     col,
		Ordinal:    ord,
		Descending: direction == tree.Descending,
	}
	ti.Columns = append(ti.Columns, idxCol)
	if makeUnique {
		// Need to add to the index's count of columns that are part of its
		// unique key.
		ti.Unique++
	}
	return col.(*Column)
}

func (tt *Table) addPrimaryColumnIndex(colName string) {
	def := tree.IndexTableDef{
		Columns: tree.IndexElemList{{Column: tree.Name(colName), Direction: tree.Ascending}},
	}
	tt.addIndex(&def, primaryIndex)
}