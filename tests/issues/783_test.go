// Licensed to ClickHouse, Inc. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. ClickHouse, Inc. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package issues

import (
	"context"
	"strconv"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	clickhouse_tests "github.com/ClickHouse/clickhouse-go/v2/tests"
	clickhouse_std_tests "github.com/ClickHouse/clickhouse-go/v2/tests/std"
	"github.com/stretchr/testify/require"
)

func Test783(t *testing.T) {
	var (
		conn, err = clickhouse_tests.GetConnectionTCP("issues", clickhouse.Settings{
			"flatten_nested": 1,
		}, nil, &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		})
	)
	ctx := context.Background()
	require.NoError(t, err)
	row := conn.QueryRow(ctx, "SELECT groupArray(('a', ['time1', 'time2'])) as val")
	var x [][]any
	require.NoError(t, row.Scan(&x))
	require.Equal(t, [][]any{{"a", []string{"time1", "time2"}}}, x)
}

func TestStd783(t *testing.T) {
	useSSL, err := strconv.ParseBool(clickhouse_tests.GetEnv("CLICKHOUSE_USE_SSL", "false"))
	require.NoError(t, err)
	conn, err := clickhouse_std_tests.GetDSNConnection("issues", clickhouse.Native, useSSL, nil)
	require.NoError(t, err)
	row := conn.QueryRow("SELECT groupArray(('a', ['time1', 'time2'])) as val")
	var x [][]any
	require.NoError(t, row.Scan(&x))
	require.Equal(t, [][]any{{"a", []string{"time1", "time2"}}}, x)
}
