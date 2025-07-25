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

package tests

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-units"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var testUUID = uuid.NewString()[0:12]
var testTimestamp = time.Now().UnixMilli()
var randSeed = time.Now().UnixNano()

const defaultClickHouseVersion = "latest"

func GetClickHouseTestVersion() string {
	return GetEnv("CLICKHOUSE_VERSION", defaultClickHouseVersion)
}

type ClickHouseTestEnvironment struct {
	ContainerID string
	Port        int
	HttpPort    int
	SslPort     int
	HttpsPort   int
	Host        string
	Username    string
	Password    string
	JWT         string
	Database    string
	Version     proto.Version
	ContainerIP string
	Container   testcontainers.Container `json:"-"`
}

func (env *ClickHouseTestEnvironment) setVersion() {
	useSSL, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_SSL", "false"))
	if err != nil {
		panic(err)
	}
	port := env.Port
	var tlsConfig *tls.Config
	if useSSL {
		tlsConfig = &tls.Config{}
		port = env.SslPort
	}
	timeout, err := strconv.Atoi(GetEnv("CLICKHOUSE_DIAL_TIMEOUT", "10"))
	if err != nil {
		panic(err)
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:     []string{fmt.Sprintf("%s:%d", env.Host, port)},
		Settings: nil,
		Auth: clickhouse.Auth{
			Database: "default",
			Username: env.Username,
			Password: env.Password,
		},
		TLS:         tlsConfig,
		DialTimeout: time.Duration(timeout) * time.Second,
	})
	if err != nil {
		panic(err)
	}
	v, err := conn.ServerVersion()
	if err != nil {
		panic(err)
	}
	env.Version = v.Version
}

func CheckMinServerServerVersion(conn driver.Conn, major, minor, patch uint64) bool {
	v, err := conn.ServerVersion()
	if err != nil {
		panic(err)
	}
	return proto.CheckMinVersion(proto.Version{
		Major: major,
		Minor: minor,
		Patch: patch,
	}, v.Version)
}

func CreateClickHouseTestEnvironment(testSet string) (ClickHouseTestEnvironment, error) {
	// create a ClickHouse Container
	ctx := context.Background()
	// attempt use docker for CI
	provider, err := testcontainers.ProviderDefault.GetProvider()
	if err != nil {
		fmt.Printf("Docker is not running and no clickhouse connections details were provided. Skipping tests: %s\n", err)
		os.Exit(0)
	}
	err = provider.Health(ctx)
	if err != nil {
		fmt.Printf("Docker is not running and no clickhouse connections details were provided. Skipping IT tests: %s\n", err)
		os.Exit(0)
	}
	fmt.Println("Using Docker for integration tests")
	_, b, _, _ := runtime.Caller(0)
	basePath := filepath.Dir(b)
	if err != nil {
		// can't test without Container
		panic(err)
	}

	expected := []*units.Ulimit{
		{
			Name: "nofile",
			Hard: 262144,
			Soft: 262144,
		},
	}

	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, time.Now().UnixNano()); err != nil {
		return ClickHouseTestEnvironment{}, err
	}
	containerName := fmt.Sprintf("clickhouse-go-%x", md5.Sum(buf.Bytes()))

	req := testcontainers.ContainerRequest{
		Image:        fmt.Sprintf("clickhouse/clickhouse-server:%s", GetClickHouseTestVersion()),
		Name:         containerName,
		ExposedPorts: []string{"9000/tcp", "8123/tcp", "9440/tcp", "8443/tcp"},
		WaitingFor: wait.ForAll(
			wait.ForListeningPort("9000/tcp"),
			wait.ForListeningPort("8123/tcp"),
			wait.ForHTTP("/").WithPort("8123/tcp"),
		).WithDeadline(time.Second * 120),
		Mounts: []testcontainers.ContainerMount{
			testcontainers.BindMount(path.Join(basePath, "./resources/custom.xml"), "/etc/clickhouse-server/config.d/custom.xml"),
			testcontainers.BindMount(path.Join(basePath, "./resources/admin.xml"), "/etc/clickhouse-server/users.d/admin.xml"),
			testcontainers.BindMount(path.Join(basePath, "./resources/clickhouse.crt"), "/etc/clickhouse-server/certs/clickhouse.crt"),
			testcontainers.BindMount(path.Join(basePath, "./resources/clickhouse.key"), "/etc/clickhouse-server/certs/clickhouse.key"),
			testcontainers.BindMount(path.Join(basePath, "./resources/CAroot.crt"), "/etc/clickhouse-server/certs/CAroot.crt"),
		},
		Resources: container.Resources{
			Ulimits: expected,
		},
	}

	var clickhouseContainer testcontainers.Container
	for attempt := 0; attempt < 3; attempt++ {
		clickhouseContainer, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err == nil {
			break
		}

		if strings.Contains(err.Error(), "failed to start container") {
			// retry
			continue
		}

		return ClickHouseTestEnvironment{}, err
	}

	p, _ := clickhouseContainer.MappedPort(ctx, "9000")
	hp, _ := clickhouseContainer.MappedPort(ctx, "8123")
	sslPort, _ := clickhouseContainer.MappedPort(ctx, "9440")
	hps, _ := clickhouseContainer.MappedPort(ctx, "8443")
	ip, _ := clickhouseContainer.ContainerIP(ctx)
	testEnv := ClickHouseTestEnvironment{
		ContainerID: clickhouseContainer.GetContainerID(),
		Port:        p.Int(),
		HttpPort:    hp.Int(),
		SslPort:     sslPort.Int(),
		HttpsPort:   hps.Int(),
		Host:        "127.0.0.1",
		Username:    "tester",
		Password:    "ClickHouse",
		Container:   clickhouseContainer,
		ContainerIP: ip,
		Database:    GetEnv("CLICKHOUSE_DATABASE", getDatabaseName(testSet)),
	}
	testEnv.setVersion()

	fmt.Printf("ClickHouse %s ready: Container=%s Host=%s TCP=%d HTTP=%d SSL=%d HTTPS=%d \n",
		testEnv.Version.String(), testEnv.ContainerID[:12], testEnv.Host, testEnv.Port, testEnv.HttpPort, testEnv.SslPort, testEnv.HttpsPort)

	return testEnv, nil
}

func SetTestEnvironment(testSet string, environment ClickHouseTestEnvironment) {
	bytes, err := json.Marshal(environment)
	if err != nil {
		panic(err)
	}
	os.Setenv(fmt.Sprintf("CLICKHOUSE_%s_ENV", strings.ToUpper(testSet)), string(bytes))
}

func GetTestEnvironment(testSet string) (ClickHouseTestEnvironment, error) {
	useDocker, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_DOCKER", "true"))
	if err != nil {
		return ClickHouseTestEnvironment{}, err
	}
	if !useDocker {
		return GetExternalTestEnvironment(testSet)
	}
	sEnv := os.Getenv(fmt.Sprintf("CLICKHOUSE_%s_ENV", strings.ToUpper(testSet)))
	if sEnv == "" {
		return ClickHouseTestEnvironment{}, errors.New("unable to find environment")
	}
	var env ClickHouseTestEnvironment
	if err := json.Unmarshal([]byte(sEnv), &env); err != nil {
		return ClickHouseTestEnvironment{}, err
	}
	return env, nil
}

func GetExternalTestEnvironment(testSet string) (ClickHouseTestEnvironment, error) {
	port, err := strconv.Atoi(GetEnv("CLICKHOUSE_PORT", "9000"))
	if err != nil {
		return ClickHouseTestEnvironment{}, nil
	}
	httpPort, err := strconv.Atoi(GetEnv("CLICKHOUSE_HTTP_PORT", "8123"))
	if err != nil {
		return ClickHouseTestEnvironment{}, nil
	}
	sslPort, err := strconv.Atoi(GetEnv("CLICKHOUSE_SSL_PORT", "9440"))
	if err != nil {
		return ClickHouseTestEnvironment{}, nil
	}
	httpsPort, err := strconv.Atoi(GetEnv("CLICKHOUSE_HTTPS_PORT", "8443"))
	if err != nil {
		return ClickHouseTestEnvironment{}, nil
	}
	env := ClickHouseTestEnvironment{
		Port:      port,
		HttpPort:  httpPort,
		SslPort:   sslPort,
		HttpsPort: httpsPort,
		Username:  GetEnv("CLICKHOUSE_USERNAME", "default"),
		Password:  GetEnv("CLICKHOUSE_PASSWORD", ""),
		JWT:       GetEnv("CLICKHOUSE_JWT", ""),
		Host:      GetEnv("CLICKHOUSE_HOST", "localhost"),
		Database:  GetEnv("CLICKHOUSE_DATABASE", getDatabaseName(testSet)),
	}
	env.setVersion()
	return env, nil
}

func ClientOptionsFromEnv(env ClickHouseTestEnvironment, settings clickhouse.Settings, useHTTP bool) clickhouse.Options {
	timeout, err := strconv.Atoi(GetEnv("CLICKHOUSE_DIAL_TIMEOUT", "10"))
	if err != nil {
		timeout = 10
	}

	useSSL, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_SSL", "false"))
	if err != nil {
		panic(err)
	}

	port := env.Port
	if useHTTP {
		port = env.HttpPort
	}
	var tlsConfig *tls.Config
	if useSSL {
		tlsConfig = &tls.Config{}
		port = env.SslPort
		if useHTTP {
			port = env.HttpsPort
		}
	}

	protocol := clickhouse.Native
	if useHTTP {
		protocol = clickhouse.HTTP
	}

	return clickhouse.Options{
		Addr:     []string{fmt.Sprintf("%s:%d", env.Host, port)},
		Protocol: protocol,
		Settings: settings,
		Auth: clickhouse.Auth{
			Database: env.Database,
			Username: env.Username,
			Password: env.Password,
		},
		DialTimeout: time.Duration(timeout) * time.Second,
		TLS:         tlsConfig,
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
	}
}

func TestClientWithDefaultOptions(env ClickHouseTestEnvironment, settings clickhouse.Settings) (driver.Conn, error) {
	opts := ClientOptionsFromEnv(env, settings, false)
	return clickhouse.Open(&opts)
}

func TestClientDefaultSettings(env ClickHouseTestEnvironment) clickhouse.Settings {
	settings := clickhouse.Settings{}

	if proto.CheckMinVersion(proto.Version{
		Major: 22,
		Minor: 8,
		Patch: 0,
	}, env.Version) {
		settings["database_replicated_enforce_synchronous_settings"] = "1"
	}
	settings["insert_quorum"], _ = strconv.Atoi(GetEnv("CLICKHOUSE_QUORUM_INSERT", "1"))
	settings["insert_quorum_parallel"] = 0
	settings["select_sequential_consistency"] = 1

	return settings
}

func TestClientWithDefaultSettings(env ClickHouseTestEnvironment) (driver.Conn, error) {
	return TestClientWithDefaultOptions(env, TestClientDefaultSettings(env))
}

func TestDatabaseSQLClientWithDefaultOptions(env ClickHouseTestEnvironment, settings clickhouse.Settings) (*sql.DB, error) {
	opts := ClientOptionsFromEnv(env, settings, false)
	return sql.Open("clickhouse", OptionsToDSN(&opts))
}

func TestDatabaseSQLClientWithDefaultSettings(env ClickHouseTestEnvironment) (*sql.DB, error) {
	return TestDatabaseSQLClientWithDefaultOptions(env, TestClientDefaultSettings(env))
}

func GetConnection(testSet string, t *testing.T, protocol clickhouse.Protocol, settings clickhouse.Settings, tlsConfig *tls.Config, compression *clickhouse.Compression) (driver.Conn, error) {
	env, err := GetTestEnvironment(testSet)
	if err != nil {
		return nil, err
	}

	switch protocol {
	case clickhouse.Native:
		return getConnection(env, env.Database, settings, tlsConfig, compression)
	case clickhouse.HTTP:
		return getHTTPConnection(env, t, env.Database, settings, tlsConfig, compression)
	default:
		return nil, fmt.Errorf("unknown protocol: %s", protocol)
	}
}

func GetConnectionTCP(testSet string, settings clickhouse.Settings, tlsConfig *tls.Config, compression *clickhouse.Compression) (driver.Conn, error) {
	env, err := GetTestEnvironment(testSet)
	if err != nil {
		return nil, err
	}

	return getConnection(env, env.Database, settings, tlsConfig, compression)
}

func GetConnectionHTTP(testSet string, t *testing.T, settings clickhouse.Settings, tlsConfig *tls.Config, compression *clickhouse.Compression) (driver.Conn, error) {
	env, err := GetTestEnvironment(testSet)
	if err != nil {
		return nil, err
	}

	return getHTTPConnection(env, t, env.Database, settings, tlsConfig, compression)
}

func GetJWTConnection(testSet string, settings clickhouse.Settings, tlsConfig *tls.Config, maxConnLifetime time.Duration, jwtFunc clickhouse.GetJWTFunc) (driver.Conn, error) {
	env, err := GetTestEnvironment(testSet)
	if err != nil {
		return nil, err
	}
	return getJWTConnection(env, env.Database, settings, tlsConfig, maxConnLifetime, jwtFunc)
}

func GetConnectionWithOptions(options *clickhouse.Options) (driver.Conn, error) {
	if options.Settings == nil {
		options.Settings = clickhouse.Settings{}
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if CheckMinServerServerVersion(conn, 22, 8, 0) {
		options.Settings["database_replicated_enforce_synchronous_settings"] = "1"
	}
	options.Settings["insert_quorum"], err = strconv.Atoi(GetEnv("CLICKHOUSE_QUORUM_INSERT", "1"))
	options.Settings["insert_quorum_parallel"] = 0
	options.Settings["select_sequential_consistency"] = 1
	if err != nil {
		return nil, err
	}
	return clickhouse.Open(options)
}

func getConnection(env ClickHouseTestEnvironment, database string, settings clickhouse.Settings, tlsConfig *tls.Config, compression *clickhouse.Compression) (driver.Conn, error) {
	useSSL, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_SSL", "false"))
	if err != nil {
		panic(err)
	}
	port := env.Port
	if useSSL && tlsConfig == nil {
		tlsConfig = &tls.Config{}
		port = env.SslPort
	}
	if settings == nil {
		settings = clickhouse.Settings{}
	}
	if proto.CheckMinVersion(proto.Version{
		Major: 22,
		Minor: 8,
		Patch: 0,
	}, env.Version) {
		settings["database_replicated_enforce_synchronous_settings"] = "1"
	}
	if proto.CheckMinVersion(proto.Version{
		Major: 25,
		Minor: 6,
		Patch: 0,
	}, env.Version) {
		settings["output_format_native_use_flattened_dynamic_and_json_serialization"] = "1"
	}
	settings["insert_quorum"], err = strconv.Atoi(GetEnv("CLICKHOUSE_QUORUM_INSERT", "1"))
	settings["insert_quorum_parallel"] = 0
	settings["select_sequential_consistency"] = 1
	if err != nil {
		return nil, err
	}

	timeout, err := strconv.Atoi(GetEnv("CLICKHOUSE_DIAL_TIMEOUT", "10"))
	if err != nil {
		return nil, err
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Protocol: clickhouse.Native,
		Addr:     []string{fmt.Sprintf("%s:%d", env.Host, port)},
		Settings: settings,
		Auth: clickhouse.Auth{
			Database: database,
			Username: env.Username,
			Password: env.Password,
		},
		TLS:         tlsConfig,
		Compression: compression,
		DialTimeout: time.Duration(timeout) * time.Second,
	})
	return conn, err
}

func getHTTPConnection(env ClickHouseTestEnvironment, t *testing.T, database string, settings clickhouse.Settings, tlsConfig *tls.Config, compression *clickhouse.Compression) (driver.Conn, error) {
	useSSL, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_SSL", "false"))
	if err != nil {
		panic(err)
	}
	port := env.HttpPort
	if useSSL && tlsConfig == nil {
		tlsConfig = &tls.Config{}
		port = env.HttpsPort
	}
	if settings == nil {
		settings = clickhouse.Settings{}
	}
	if proto.CheckMinVersion(proto.Version{
		Major: 22,
		Minor: 8,
		Patch: 0,
	}, env.Version) {
		settings["database_replicated_enforce_synchronous_settings"] = "1"
	}
	if proto.CheckMinVersion(proto.Version{
		Major: 25,
		Minor: 6,
		Patch: 0,
	}, env.Version) {
		settings["output_format_native_use_flattened_dynamic_and_json_serialization"] = "1"
	}
	settings["insert_quorum"], err = strconv.Atoi(GetEnv("CLICKHOUSE_QUORUM_INSERT", "1"))
	settings["insert_quorum_parallel"] = 0
	settings["select_sequential_consistency"] = 1
	if err != nil {
		return nil, err
	}

	timeout, err := strconv.Atoi(GetEnv("CLICKHOUSE_DIAL_TIMEOUT", "10"))
	if err != nil {
		return nil, err
	}

	// Each test uses its own session ID.
	// This may be problematic on some tests, but overall it is more consistent.
	settings["session_id"] = t.Name()

	conn, err := clickhouse.Open(&clickhouse.Options{
		Protocol: clickhouse.HTTP,
		Addr:     []string{fmt.Sprintf("%s:%d", env.Host, port)},
		Settings: settings,
		Auth: clickhouse.Auth{
			Database: database,
			Username: env.Username,
			Password: env.Password,
		},
		TLS:                 tlsConfig,
		Compression:         compression,
		DialTimeout:         time.Duration(timeout) * time.Second,
		ConnMaxLifetime:     1 * time.Second,
		MaxOpenConns:        1,
		MaxIdleConns:        1,
		HttpMaxConnsPerHost: 1,
	})
	return conn, err
}

func getJWTConnection(env ClickHouseTestEnvironment, database string, settings clickhouse.Settings, tlsConfig *tls.Config, maxConnLifetime time.Duration, jwtFunc clickhouse.GetJWTFunc) (driver.Conn, error) {
	useSSL, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_SSL", "false"))
	if err != nil {
		panic(err)
	}
	port := env.Port
	if useSSL && tlsConfig == nil {
		tlsConfig = &tls.Config{}
		port = env.SslPort
	}
	if settings == nil {
		settings = clickhouse.Settings{}
	}
	if proto.CheckMinVersion(proto.Version{
		Major: 22,
		Minor: 8,
		Patch: 0,
	}, env.Version) {
		settings["database_replicated_enforce_synchronous_settings"] = "1"
	}
	settings["insert_quorum"], err = strconv.Atoi(GetEnv("CLICKHOUSE_QUORUM_INSERT", "1"))
	settings["insert_quorum_parallel"] = 0
	settings["select_sequential_consistency"] = 1
	if err != nil {
		return nil, err
	}

	timeout, err := strconv.Atoi(GetEnv("CLICKHOUSE_DIAL_TIMEOUT", "10"))
	if err != nil {
		return nil, err
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:     []string{fmt.Sprintf("%s:%d", env.Host, port)},
		Settings: settings,
		Auth: clickhouse.Auth{
			Database: database,
		},
		GetJWT:          jwtFunc,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: maxConnLifetime,
		TLS:             tlsConfig,
		Compression:     nil,
		DialTimeout:     time.Duration(timeout) * time.Second,
	})
	return conn, err
}

func CreateDatabase(testSet string) error {
	env, err := GetTestEnvironment(testSet)
	if err != nil {
		return err
	}
	conn, err := getConnection(env, "default", nil, nil, nil)
	if err != nil {
		return err
	}
	return conn.Exec(context.Background(), fmt.Sprintf("CREATE DATABASE `%s`", env.Database))
}

const (
	readOnlyReadWriteChangeSettings = 0
	readOnlyRead                    = 1
	readOnlyReadChangeSettings      = 2
)

func createUserWithReadOnlySetting(conn driver.Conn, defaultDatabase string, readOnlyType int) (username, password string, err error) {
	username = fmt.Sprintf("readonly_user_%s", RandAsciiString(6))
	password = RandAsciiString(10) + "1#"

	createUserQuery := fmt.Sprintf(`
          CREATE USER IF NOT EXISTS %s 
          IDENTIFIED BY '%s'
          DEFAULT DATABASE "%s"
          SETTINGS readonly = %d
        `, username, password, defaultDatabase, readOnlyType)
	if err := conn.Exec(context.Background(), createUserQuery); err != nil {
		return "", "", err
	}

	grantQuery := fmt.Sprintf(`
          GRANT SELECT, INSERT, CREATE TABLE, DROP TABLE 
          ON "%s".*
          TO %s
        `, defaultDatabase, username)

	return username, password, conn.Exec(context.Background(), grantQuery)
}

func dropUser(conn driver.Conn, username string) error {
	query := fmt.Sprintf(`
          DROP USER IF EXISTS %s
        `, username)

	return conn.Exec(context.Background(), query)
}

func createSimpleTable(client driver.Conn, table string) error {
	return client.Exec(context.Background(), fmt.Sprintf(`
		CREATE TABLE %s (
			  Col1 UInt8
		) Engine MergeTree() ORDER BY tuple()
	`, table))
}

func dropTable(client driver.Conn, table string) error {
	return client.Exec(context.Background(), fmt.Sprintf(`
		DROP TABLE %s
	`, table))
}

func getDatabaseName(testSet string) string {
	return fmt.Sprintf("clickhouse-go-%s-%s-%d", testSet, testUUID, testTimestamp)
}

func getRowsCount(t *testing.T, conn driver.Conn, table string) uint64 {
	var count uint64
	err := conn.QueryRow(context.Background(), fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&count)
	require.NoError(t, err)
	return count
}

func deduplicateTable(t *testing.T, conn driver.Conn, table string) {
	require.NoError(t, conn.Exec(context.Background(), fmt.Sprintf(`OPTIMIZE TABLE %s DEDUPLICATE`, table)))
}

func GetEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func IsSetInEnv(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

var src = rand.NewSource(time.Now().UnixNano())

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
const numberBytes = "123456789"
const (
	letterIdxBits = 6                    // 6 bits to represent a letter index
	letterIdxMask = 1<<letterIdxBits - 1 // All 1-bits, as many as letterIdxBits
	letterIdxMax  = 63 / letterIdxBits   // # of letter indices fitting in 63 bits
)

func RandAsciiString(n int) string {
	return randString(n, letterBytes)
}

func RandIntString(n int) string {
	return randString(n, numberBytes)
}

func RandIPv4() net.IP {
	return net.IPv4(uint8(rand.Int()), uint8(rand.Int()), uint8(rand.Int()), uint8(rand.Int())).To4()
}

func RandIPv6() net.IP {
	size := 16
	ip := make([]byte, size)
	for i := 0; i < size; i++ {
		ip[i] = byte(rand.Intn(256))
	}
	return net.IP(ip).To16()
}

func randString(n int, bytes string) string {
	sb := strings.Builder{}
	sb.Grow(n)
	// A src.Int63() generates 63 random bits, enough for letterIdxMax characters!
	for i, cache, remain := n-1, src.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = src.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(bytes) {
			sb.WriteByte(bytes[idx])
			i--
		}
		cache >>= letterIdxBits
		remain--
	}

	return sb.String()
}

// PrintMemUsage outputs the current, total and OS memory being used. As well as the number
// of garbage collection cycles completed.
// thanks to https://golangcode.com/print-the-current-memory-usage/
func PrintMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// For info on each, see: https://golang.org/pkg/runtime/#MemStats
	fmt.Printf("Alloc = %v MiB", bToMb(m.Alloc))
	fmt.Printf("\tTotalAlloc = %v MiB", bToMb(m.TotalAlloc))
	fmt.Printf("\tSys = %v MiB", bToMb(m.Sys))
	fmt.Printf("\tNumGC = %v\n", m.NumGC)
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

type NginxReverseHTTPProxyTestEnvironment struct {
	HttpPort       int
	NginxContainer testcontainers.Container `json:"-"`
}

func CreateNginxReverseProxyTestEnvironment(clickhouseEnv ClickHouseTestEnvironment) (NginxReverseHTTPProxyTestEnvironment, error) {
	// create a nginx Container as a reverse proxy
	ctx := context.Background()
	nginxReq := testcontainers.ContainerRequest{
		Image:        "nginx",
		Name:         fmt.Sprintf("nginx-clickhouse-go-%d", time.Now().UnixNano()),
		ExposedPorts: []string{"80/tcp"},
		WaitingFor:   wait.ForListeningPort("80/tcp").WithStartupTimeout(time.Second * time.Duration(120)),
		Cmd:          []string{},
	}
	nginxContainer, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: nginxReq,
		Started:          true,
	})
	if err != nil {
		return NginxReverseHTTPProxyTestEnvironment{}, err
	}
	_, b, _, _ := runtime.Caller(0)
	basePath := filepath.Dir(b)
	nginxConf, err := os.ReadFile(path.Join(basePath, "./resources/nginx.conf"))
	if err != nil {
		return NginxReverseHTTPProxyTestEnvironment{}, err
	}
	// replace upstream clickhouse endpoint
	nginxConf = []byte(strings.Replace(string(nginxConf), "<upstream_http_endpoint>", fmt.Sprintf("%v:8123", clickhouseEnv.ContainerIP), -1))
	err = nginxContainer.CopyToContainer(ctx, nginxConf, "/etc/nginx/nginx.conf", 700)
	if err != nil {
		return NginxReverseHTTPProxyTestEnvironment{}, err
	}
	// reload new nginx.conf and set http proxy upstream
	_, _, err = nginxContainer.Exec(ctx, []string{"nginx", "-s", "reload"})
	if err != nil {
		return NginxReverseHTTPProxyTestEnvironment{}, err
	}
	nginxReloadWaiter := wait.ForHTTP("/clickhouse").WithStartupTimeout(time.Second * time.Duration(120))
	err = nginxReloadWaiter.WaitUntilReady(ctx, nginxContainer)
	if err != nil {
		return NginxReverseHTTPProxyTestEnvironment{}, err
	}
	p, _ := nginxContainer.MappedPort(ctx, "80")
	return NginxReverseHTTPProxyTestEnvironment{
		HttpPort:       p.Int(),
		NginxContainer: nginxContainer,
	}, nil
}

type TinyProxyTestEnvironment struct {
	HttpPort  int
	Container testcontainers.Container `json:"-"`
}

func (e TinyProxyTestEnvironment) ProxyUrl(t *testing.T) string {
	require.NotNil(t, e.Container)

	host, err := e.Container.Host(context.Background())
	require.NoError(t, err)

	return fmt.Sprintf("http://%s:%d", host, e.HttpPort)
}

func CreateTinyProxyTestEnvironment(t *testing.T) (TinyProxyTestEnvironment, error) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "monokal/tinyproxy",
		Name:         fmt.Sprintf("tinyproxy-clickhouse-go-%d", time.Now().UnixNano()),
		ExposedPorts: []string{"8888/tcp"},
		WaitingFor:   wait.ForListeningPort("8888/tcp").WithStartupTimeout(time.Second * time.Duration(120)),
		Cmd:          []string{"--enable-debug", "ANY"},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	p, _ := container.MappedPort(ctx, "8888")
	return TinyProxyTestEnvironment{
		HttpPort:  p.Int(),
		Container: container,
	}, nil
}

func TestProtocols(rootT *testing.T, testFunc func(t *testing.T, protocol clickhouse.Protocol)) {
	rootT.Run("Native", func(t *testing.T) {
		testFunc(t, clickhouse.Native)
	})
	rootT.Run("HTTP", func(t *testing.T) {
		testFunc(t, clickhouse.HTTP)
	})
}

func CleanupNativeConn(t *testing.T, conn driver.Conn) {
	t.Cleanup(func() {
		if conn == nil {
			return
		}

		if err := conn.Close(); err != nil {
			t.Log(fmt.Errorf("failed to close connection: %w", err))
		}
	})
}

func OptionsToDSN(o *clickhouse.Options) string {
	var u url.URL

	if o.Protocol == clickhouse.Native {
		u.Scheme = "clickhouse"
	} else {
		if o.TLS != nil {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
	}

	u.Host = strings.Join(o.Addr, ",")
	u.User = url.UserPassword(o.Auth.Username, o.Auth.Password)
	u.Path = fmt.Sprintf("/%s", o.Auth.Database)

	params := u.Query()

	if o.TLS != nil {
		params.Set("secure", "true")
	}

	if o.TLS != nil && o.TLS.InsecureSkipVerify {
		params.Set("skip_verify", "true")
	}

	if o.Debug {
		params.Set("debug", "true")
	}

	if o.Compression != nil {
		params.Set("compress", o.Compression.Method.String())
		if o.Compression.Level > 0 {
			params.Set("compress_level", strconv.Itoa(o.Compression.Level))
		}
	}

	if o.MaxCompressionBuffer > 0 {
		params.Set("max_compression_buffer", strconv.Itoa(o.MaxCompressionBuffer))
	}

	if o.DialTimeout > 0 {
		params.Set("dial_timeout", o.DialTimeout.String())
	}

	if o.BlockBufferSize > 0 {
		params.Set("block_buffer_size", strconv.Itoa(int(o.BlockBufferSize)))
	}

	if o.ReadTimeout > 0 {
		params.Set("read_timeout", o.ReadTimeout.String())
	}

	if o.ConnOpenStrategy != 0 {
		var strategy string
		switch o.ConnOpenStrategy {
		case clickhouse.ConnOpenInOrder:
			strategy = "in_order"
		case clickhouse.ConnOpenRoundRobin:
			strategy = "round_robin"
		case clickhouse.ConnOpenRandom:
			strategy = "random"
		}

		params.Set("connection_open_strategy", strategy)
	}

	if o.MaxOpenConns > 0 {
		params.Set("max_open_conns", strconv.Itoa(o.MaxOpenConns))
	}

	if o.MaxIdleConns > 0 {
		params.Set("max_idle_conns", strconv.Itoa(o.MaxIdleConns))
	}

	if o.ConnMaxLifetime > 0 {
		params.Set("conn_max_lifetime", o.ConnMaxLifetime.String())
	}

	if o.ClientInfo.Products != nil {
		var products []string
		for _, product := range o.ClientInfo.Products {
			products = append(products, fmt.Sprintf("%s/%s", product.Name, product.Version))
		}
		params.Set("client_info_product", strings.Join(products, ","))
	}

	for k, v := range o.Settings {
		switch v := v.(type) {
		case bool:
			if v {
				params.Set(k, "true")
			} else {
				params.Set(k, "false")
			}
		case int:
			params.Set(k, strconv.Itoa(v))
		case string:
			params.Set(k, v)
		}
	}

	u.RawQuery = params.Encode()

	return u.String()
}

func ResetRandSeed() {
	rand.Seed(randSeed)
}

func Runtime(m *testing.M, ts string) (exitCode int) {
	ResetRandSeed()
	fmt.Printf("using random seed %d for %s tests\n", randSeed, ts)

	useDocker, err := strconv.ParseBool(GetEnv("CLICKHOUSE_USE_DOCKER", "true"))
	if err != nil {
		panic(err)
	}

	var env ClickHouseTestEnvironment
	switch useDocker {
	case true:
		env, err = CreateClickHouseTestEnvironment(ts)
		if err != nil {
			panic(err)
		}
		defer func() {
			if err := env.Container.Terminate(context.Background()); err != nil {
				panic(err)
			}
		}() //nolint
	case false:
		env, err = GetExternalTestEnvironment(ts)
		if err != nil {
			panic(err)
		}
	}

	SetTestEnvironment(ts, env)
	if err := CreateDatabase(ts); err != nil {
		panic(err)
	}

	return m.Run()
}
