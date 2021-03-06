// Copyright 2019 Liquidata, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package merge

import (
	"context"
	"io"

	"github.com/liquidata-inc/dolt/go/libraries/doltcore/doltdb"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/row"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/rowconv"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/schema"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/pipeline"
	"github.com/liquidata-inc/dolt/go/libraries/doltcore/table/untyped"
	"github.com/liquidata-inc/dolt/go/store/types"
)

const (
	mergeVersionProp  = "merge_version"
	mergeRowOperation = "row_operation"
)

type MergeVersion int

const (
	BaseVersion MergeVersion = iota
	OurVersion
	TheirVersion
	Blank // for display only
)

type ConflictReader struct {
	confItr      types.MapIterator
	unionedSch   schema.Schema
	baseConv     *rowconv.RowConverter
	conv         *rowconv.RowConverter
	mergeConv    *rowconv.RowConverter
	bufferedRows [3]pipeline.RowWithProps
	currIdx      int
}

func NewConflictReader(ctx context.Context, tbl *doltdb.Table) (*ConflictReader, error) {
	base, sch, mergeSch, err := tbl.GetConflictSchemas(ctx)

	if err != nil {
		return nil, err
	}

	untypedUnSch, err := untyped.UntypedSchemaUnion(base, sch, mergeSch)

	if err != nil {
		return nil, err
	}

	var baseMapping, mapping, mergeMapping *rowconv.FieldMapping
	baseMapping, err = rowconv.TagMapping(base, untypedUnSch)

	if err != nil {
		return nil, err
	}

	mapping, err = rowconv.TagMapping(sch, untypedUnSch)

	if err != nil {
		return nil, err
	}

	mergeMapping, err = rowconv.TagMapping(mergeSch, untypedUnSch)

	if err != nil {
		return nil, err
	}

	_, confData, err := tbl.GetConflicts(ctx)

	if err != nil {
		return nil, err
	}

	confItr, err := confData.Iterator(ctx)

	if err != nil {
		return nil, err
	}

	baseConv, err := rowconv.NewRowConverter(baseMapping)

	if err != nil {
		return nil, err
	}

	conv, err := rowconv.NewRowConverter(mapping)

	if err != nil {
		return nil, err
	}

	mergeConv, err := rowconv.NewRowConverter(mergeMapping)

	if err != nil {
		return nil, err
	}

	return &ConflictReader{
		confItr,
		untypedUnSch,
		baseConv,
		conv,
		mergeConv,
		[3]pipeline.RowWithProps{},
		0}, nil
}

// GetSchema gets the schema of the rows that this reader will return
func (cr *ConflictReader) GetSchema() schema.Schema {
	return cr.unionedSch
}

// NextConflict reads a row from a table.  If there is a bad row the returned error will be non nil, and callin IsBadRow(err)
// will be return true. This is a potentially non-fatal error and callers can decide if they want to continue on a bad row, or fail.
func (cr *ConflictReader) NextConflict(ctx context.Context) (row.Row, pipeline.ImmutableProperties, error) {
	for {
		if cr.currIdx == 0 {
			key, value, err := cr.confItr.Next(ctx)

			if err != nil {
				return nil, pipeline.ImmutableProperties{}, err
			}

			if key == nil {
				return nil, pipeline.NoProps, io.EOF
			}

			keyTpl := key.(types.Tuple)
			conflict, err := doltdb.ConflictFromTuple(value.(types.Tuple))

			if err != nil {
				return nil, pipeline.ImmutableProperties{}, err
			}

			baseRow, err := createRow(keyTpl, conflict.Base, cr.baseConv)

			if err != nil {
				return nil, pipeline.ImmutableProperties{}, err
			}

			r, err := createRow(keyTpl, conflict.Value, cr.conv)

			if err != nil {
				return nil, pipeline.ImmutableProperties{}, err
			}

			mergeRow, err := createRow(keyTpl, conflict.MergeValue.(types.Tuple), cr.mergeConv)

			if err != nil {
				return nil, pipeline.ImmutableProperties{}, err
			}

			if baseRow != nil {
				if mergeRow != nil && r != nil {
					cr.bufferedRows[2] = pipeline.NewRowWithProps(baseRow, map[string]interface{}{mergeVersionProp: BaseVersion})
					cr.bufferedRows[1] = pipeline.NewRowWithProps(mergeRow, map[string]interface{}{mergeVersionProp: TheirVersion, mergeRowOperation: types.DiffChangeModified})
					cr.bufferedRows[0] = pipeline.NewRowWithProps(r, map[string]interface{}{mergeVersionProp: OurVersion, mergeRowOperation: types.DiffChangeModified})
					cr.currIdx = 3
				} else if r != nil {
					cr.bufferedRows[2] = pipeline.NewRowWithProps(baseRow, map[string]interface{}{mergeVersionProp: BaseVersion})
					cr.bufferedRows[1] = pipeline.NewRowWithProps(baseRow, map[string]interface{}{mergeVersionProp: TheirVersion, mergeRowOperation: types.DiffChangeRemoved})
					cr.bufferedRows[0] = pipeline.NewRowWithProps(r, map[string]interface{}{mergeVersionProp: OurVersion, mergeRowOperation: types.DiffChangeModified})
					cr.currIdx = 3
				} else {
					cr.bufferedRows[2] = pipeline.NewRowWithProps(baseRow, map[string]interface{}{mergeVersionProp: BaseVersion})
					cr.bufferedRows[1] = pipeline.NewRowWithProps(mergeRow, map[string]interface{}{mergeVersionProp: TheirVersion, mergeRowOperation: types.DiffChangeModified})
					cr.bufferedRows[0] = pipeline.NewRowWithProps(baseRow, map[string]interface{}{mergeVersionProp: OurVersion, mergeRowOperation: types.DiffChangeRemoved})
					cr.currIdx = 3
				}
			} else {
				if mergeRow != nil {
					cr.bufferedRows[0] = pipeline.NewRowWithProps(mergeRow, map[string]interface{}{mergeVersionProp: TheirVersion, mergeRowOperation: types.DiffChangeAdded})
					cr.currIdx++
				}

				if r != nil {
					cr.bufferedRows[1] = pipeline.NewRowWithProps(r, map[string]interface{}{mergeVersionProp: OurVersion, mergeRowOperation: types.DiffChangeAdded})
					cr.currIdx++
				}
			}
		}

		cr.currIdx--
		result := cr.bufferedRows[cr.currIdx]

		if result.Row != nil {
			return result.Row, result.Props, nil
		}
	}
}

func createRow(key types.Tuple, nonKey types.Value, rowConv *rowconv.RowConverter) (row.Row, error) {
	if types.IsNull(nonKey) {
		return nil, nil
	}

	srcData, err := row.FromNoms(rowConv.SrcSch, key, nonKey.(types.Tuple))

	if err != nil {
		return nil, err
	}

	row, err := rowConv.Convert(srcData)

	if err != nil {
		return nil, err
	}

	return row, nil
}

// Close should release resources being held
func (cr *ConflictReader) Close() error {
	return nil
}
