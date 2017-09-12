package pgx

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/jackc/pgx/pgio"
	"github.com/jackc/pgx/pgproto3"
	"github.com/jackc/pgx/pgtype"
)

const (
	connStatusUninitialized = iota
	connStatusClosed
	connStatusIdle
	connStatusBusy
)

// minimalConnInfo has just enough static type information to establish the
// connection and retrieve the type data.
var minimalConnInfo *pgtype.ConnInfo

func init() {
	minimalConnInfo = pgtype.NewConnInfo()
	minimalConnInfo.InitializeDataTypes(map[string]pgtype.OID{
		"int4": pgtype.Int4OID,
		"name": pgtype.NameOID,
		"oid":  pgtype.OIDOID,
		"text": pgtype.TextOID,
	})
}

// NoticeHandler is a function that can handle notices received from the
// PostgreSQL server. Notices can be received at any time, usually during
// handling of a query response. The *Conn is provided so the handler is aware
// of the origin of the notice, but it must not invoke any query method. Be
// aware that this is distinct from LISTEN/NOTIFY notification.
type NoticeHandler func(*Conn, *Notice)

// DialFunc is a function that can be used to connect to a PostgreSQL server
type DialFunc func(network, addr string) (net.Conn, error)

// ConnConfig contains all the options used to establish a connection.
type ConnConfig struct {
	Host              string // host (e.g. localhost) or path to unix domain socket directory (e.g. /private/tmp)
	Port              uint16 // default: 5432
	Database          string
	User              string // default: OS user name
	Password          string
	TLSConfig         *tls.Config // config for TLS connection -- nil disables TLS
	UseFallbackTLS    bool        // Try FallbackTLSConfig if connecting with TLSConfig fails. Used for preferring TLS, but allowing unencrypted, or vice-versa
	FallbackTLSConfig *tls.Config // config for fallback TLS connection (only used if UseFallBackTLS is true)-- nil disables TLS
	Logger            Logger
	LogLevel          int
	Dial              DialFunc
	RuntimeParams     map[string]string // Run-time parameters to set on connection as session default values (e.g. search_path or application_name)
	OnNotice          NoticeHandler     // Callback function called when a notice response is received.
}

func (cc *ConnConfig) networkAddress() (network, address string) {
	network = "tcp"
	address = fmt.Sprintf("%s:%d", cc.Host, cc.Port)
	// See if host is a valid path, if yes connect with a socket
	if _, err := os.Stat(cc.Host); err == nil {
		// For backward compatibility accept socket file paths -- but directories are now preferred
		network = "unix"
		address = cc.Host
		if !strings.Contains(address, "/.s.PGSQL.") {
			address = filepath.Join(address, ".s.PGSQL.") + strconv.FormatInt(int64(cc.Port), 10)
		}
	}

	return network, address
}

// Conn is a PostgreSQL connection handle. It is not safe for concurrent usage.
// Use ConnPool to manage access to multiple database connections from multiple
// goroutines.
type Conn struct {
	conn               net.Conn  // the underlying TCP or unix domain socket connection
	lastActivityTime   time.Time // the last time the connection was used
	wbuf               []byte
	pid                uint32            // backend pid
	secretKey          uint32            // key to use to send a cancel query message to the server
	RuntimeParams      map[string]string // parameters that have been reported by the server
	config             ConnConfig        // config used when establishing this connection
	txStatus           byte
	preparedStatements map[string]*PreparedStatement
	channels           map[string]struct{}
	notifications      []*Notification
	logger             Logger
	logLevel           int
	fp                 *fastpath
	poolResetCount     int
	preallocatedRows   []Rows
	onNotice           NoticeHandler

	mux          sync.Mutex
	status       byte // One of connStatus* constants
	causeOfDeath error

	pendingReadyForQueryCount int // numer of ReadyForQuery messages expected
	cancelQueryInProgress     int32
	cancelQueryCompleted      chan struct{}

	// context support
	ctxInProgress bool
	doneChan      chan struct{}
	closedChan    chan error

	ConnInfo *pgtype.ConnInfo

	frontend *pgproto3.Frontend
}

// PreparedStatement is a description of a prepared statement
type PreparedStatement struct {
	Name              string
	SQL               string
	FieldDescriptions []FieldDescription
	ParameterOIDs     []pgtype.OID
}

// PrepareExOptions is an option struct that can be passed to PrepareEx
type PrepareExOptions struct {
	ParameterOIDs []pgtype.OID
}

// Notification is a message received from the PostgreSQL LISTEN/NOTIFY system
type Notification struct {
	PID     uint32 // backend pid that sent the notification
	Channel string // channel from which notification was received
	Payload string
}

// CommandTag is the result of an Exec function
type CommandTag string

// RowsAffected returns the number of rows affected. If the CommandTag was not
// for a row affecting command (such as "CREATE TABLE") then it returns 0
func (ct CommandTag) RowsAffected() int64 {
	s := string(ct)
	index := strings.LastIndex(s, " ")
	if index == -1 {
		return 0
	}
	n, _ := strconv.ParseInt(s[index+1:], 10, 64)
	return n
}

// Identifier a PostgreSQL identifier or name. Identifiers can be composed of
// multiple parts such as ["schema", "table"] or ["table", "column"].
type Identifier []string

// Sanitize returns a sanitized string safe for SQL interpolation.
func (ident Identifier) Sanitize() string {
	parts := make([]string, len(ident))
	for i := range ident {
		parts[i] = `"` + strings.Replace(ident[i], `"`, `""`, -1) + `"`
	}
	return strings.Join(parts, ".")
}

// ErrNoRows occurs when rows are expected but none are returned.
var ErrNoRows = errors.New("no rows in result set")

// ErrDeadConn occurs on an attempt to use a dead connection
var ErrDeadConn = errors.New("conn is dead")

// ErrTLSRefused occurs when the connection attempt requires TLS and the
// PostgreSQL server refuses to use TLS
var ErrTLSRefused = errors.New("server refused TLS connection")

// ErrConnBusy occurs when the connection is busy (for example, in the middle of
// reading query results) and another action is attempted.
var ErrConnBusy = errors.New("conn is busy")

// ErrInvalidLogLevel occurs on attempt to set an invalid log level.
var ErrInvalidLogLevel = errors.New("invalid log level")

// ProtocolError occurs when unexpected data is received from PostgreSQL
type ProtocolError string

func (e ProtocolError) Error() string {
	return string(e)
}

// Connect establishes a connection with a PostgreSQL server using config.
// config.Host must be specified. config.User will default to the OS user name.
// Other config fields are optional.
func Connect(config ConnConfig) (c *Conn, err error) {
	return connect(config, minimalConnInfo)
}

func connect(config ConnConfig, connInfo *pgtype.ConnInfo) (c *Conn, err error) {
	c = new(Conn)

	c.config = config
	c.ConnInfo = connInfo

	if c.config.LogLevel != 0 {
		c.logLevel = c.config.LogLevel
	} else {
		// Preserve pre-LogLevel behavior by defaulting to LogLevelDebug
		c.logLevel = LogLevelDebug
	}
	c.logger = c.config.Logger

	if c.config.User == "" {
		user, err := user.Current()
		if err != nil {
			return nil, err
		}
		c.config.User = user.Username
		if c.shouldLog(LogLevelDebug) {
			c.log(LogLevelDebug, "Using default connection config", map[string]interface{}{"User": c.config.User})
		}
	}

	if c.config.Port == 0 {
		c.config.Port = 5432
		if c.shouldLog(LogLevelDebug) {
			c.log(LogLevelDebug, "Using default connection config", map[string]interface{}{"Port": c.config.Port})
		}
	}

	c.onNotice = config.OnNotice

	network, address := c.config.networkAddress()
	if c.config.Dial == nil {
		c.config.Dial = (&net.Dialer{KeepAlive: 5 * time.Minute}).Dial
	}

	if c.shouldLog(LogLevelInfo) {
		c.log(LogLevelInfo, "Dialing PostgreSQL server", map[string]interface{}{"network": network, "address": address})
	}
	err = c.connect(config, network, address, config.TLSConfig)
	if err != nil && config.UseFallbackTLS {
		if c.shouldLog(LogLevelInfo) {
			c.log(LogLevelInfo, "connect with TLSConfig failed, trying FallbackTLSConfig", map[string]interface{}{"err": err})
		}
		err = c.connect(config, network, address, config.FallbackTLSConfig)
	}

	if err != nil {
		if c.shouldLog(LogLevelError) {
			c.log(LogLevelError, "connect failed", map[string]interface{}{"err": err})
		}
		return nil, err
	}

	return c, nil
}

func (c *Conn) connect(config ConnConfig, network, address string, tlsConfig *tls.Config) (err error) {
	c.conn, err = c.config.Dial(network, address)
	if err != nil {
		return err
	}
	defer func() {
		if c != nil && err != nil {
			c.conn.Close()
			c.mux.Lock()
			c.status = connStatusClosed
			c.mux.Unlock()
		}
	}()

	c.RuntimeParams = make(map[string]string)
	c.preparedStatements = make(map[string]*PreparedStatement)
	c.channels = make(map[string]struct{})
	c.lastActivityTime = time.Now()
	c.cancelQueryCompleted = make(chan struct{}, 1)
	c.doneChan = make(chan struct{})
	c.closedChan = make(chan error)
	c.wbuf = make([]byte, 0, 1024)

	c.mux.Lock()
	c.status = connStatusIdle
	c.mux.Unlock()

	if tlsConfig != nil {
		if c.shouldLog(LogLevelDebug) {
			c.log(LogLevelDebug, "starting TLS handshake", nil)
		}
		if err := c.startTLS(tlsConfig); err != nil {
			return err
		}
	}

	c.frontend, err = pgproto3.NewFrontend(c.conn, c.conn)
	if err != nil {
		return err
	}

	startupMsg := pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      make(map[string]string),
	}

	// Default to disabling TLS renegotiation.
	//
	// Go does not support (https://github.com/golang/go/issues/5742)
	// PostgreSQL recommends disabling (http://www.postgresql.org/docs/9.4/static/runtime-config-connection.html#GUC-SSL-RENEGOTIATION-LIMIT)
	if tlsConfig != nil {
		startupMsg.Parameters["ssl_renegotiation_limit"] = "0"
	}

	// Copy default run-time params
	for k, v := range config.RuntimeParams {
		startupMsg.Parameters[k] = v
	}

	startupMsg.Parameters["user"] = c.config.User
	if c.config.Database != "" {
		startupMsg.Parameters["database"] = c.config.Database
	}

	if _, err := c.conn.Write(startupMsg.Encode(nil)); err != nil {
		return err
	}

	c.pendingReadyForQueryCount = 1

	for {
		msg, err := c.rxMsg()
		if err != nil {
			return err
		}

		switch msg := msg.(type) {
		case *pgproto3.BackendKeyData:
			c.rxBackendKeyData(msg)
		case *pgproto3.Authentication:
			if err = c.rxAuthenticationX(msg); err != nil {
				return err
			}
		case *pgproto3.ReadyForQuery:
			c.rxReadyForQuery(msg)
			if c.shouldLog(LogLevelInfo) {
				c.log(LogLevelInfo, "connection established", nil)
			}

			// Replication connections can't execute the queries to
			// populate the c.PgTypes and c.pgsqlAfInet
			if _, ok := config.RuntimeParams["replication"]; ok {
				return nil
			}

			if c.ConnInfo == minimalConnInfo {
				err = c.initConnInfo()
				if err != nil {
					return err
				}
			}

			return nil
		default:
			if err = c.processContextFreeMsg(msg); err != nil {
				return err
			}
		}
	}
}

func (c *Conn) initConnInfo() error {
	nameOIDs := make(map[string]pgtype.OID, 256)

	rows, err := c.Query(`select t.oid, t.typname
from pg_type t
left join pg_type base_type on t.typelem=base_type.oid
where (
	  t.typtype in('b', 'p', 'r', 'e')
	  and (base_type.oid is null or base_type.typtype in('b', 'p', 'r'))
	)`)
	if err != nil {
		return err
	}

	for rows.Next() {
		var oid pgtype.OID
		var name pgtype.Text
		if err := rows.Scan(&oid, &name); err != nil {
			return err
		}

		nameOIDs[name.String] = oid
	}

	if rows.Err() != nil {
		return rows.Err()
	}

	c.ConnInfo = pgtype.NewConnInfo()
	c.ConnInfo.InitializeDataTypes(nameOIDs)
	return nil
}

// PID returns the backend PID for this connection.
func (c *Conn) PID() uint32 {
	return c.pid
}

// Close closes a connection. It is safe to call Close on a already closed
// connection.
func (c *Conn) Close() (err error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.status < connStatusIdle {
		return nil
	}
	c.status = connStatusClosed

	defer func() {
		c.conn.Close()
		c.causeOfDeath = errors.New("Closed")
		if c.shouldLog(LogLevelInfo) {
			c.log(LogLevelInfo, "closed connection", nil)
		}
	}()

	err = c.conn.SetDeadline(time.Time{})
	if err != nil && c.shouldLog(LogLevelWarn) {
		c.log(LogLevelWarn, "failed to clear deadlines to send close message", map[string]interface{}{"err": err})
		return err
	}

	_, err = c.conn.Write([]byte{'X', 0, 0, 0, 4})
	if err != nil && c.shouldLog(LogLevelWarn) {
		c.log(LogLevelWarn, "failed to send terminate message", map[string]interface{}{"err": err})
		return err
	}

	err = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err != nil && c.shouldLog(LogLevelWarn) {
		c.log(LogLevelWarn, "failed to set read deadline to finish closing", map[string]interface{}{"err": err})
		return err
	}

	_, err = c.conn.Read(make([]byte, 1))
	if err != io.EOF {
		return err
	}

	return nil
}

// Merge returns a new ConnConfig with the attributes of old and other
// combined. When an attribute is set on both, other takes precedence.
//
// As a security precaution, if the other TLSConfig is nil, all old TLS
// attributes will be preserved.
func (old ConnConfig) Merge(other ConnConfig) ConnConfig {
	cc := old

	if other.Host != "" {
		cc.Host = other.Host
	}
	if other.Port != 0 {
		cc.Port = other.Port
	}
	if other.Database != "" {
		cc.Database = other.Database
	}
	if other.User != "" {
		cc.User = other.User
	}
	if other.Password != "" {
		cc.Password = other.Password
	}

	if other.TLSConfig != nil {
		cc.TLSConfig = other.TLSConfig
		cc.UseFallbackTLS = other.UseFallbackTLS
		cc.FallbackTLSConfig = other.FallbackTLSConfig
	}

	if other.Logger != nil {
		cc.Logger = other.Logger
	}
	if other.LogLevel != 0 {
		cc.LogLevel = other.LogLevel
	}

	if other.Dial != nil {
		cc.Dial = other.Dial
	}

	cc.RuntimeParams = make(map[string]string)
	for k, v := range old.RuntimeParams {
		cc.RuntimeParams[k] = v
	}
	for k, v := range other.RuntimeParams {
		cc.RuntimeParams[k] = v
	}

	return cc
}

// ParseURI parses a database URI into ConnConfig
//
// Query parameters not used by the connection process are parsed into ConnConfig.RuntimeParams.
func ParseURI(uri string) (ConnConfig, error) {
	var cp ConnConfig

	url, err := url.Parse(uri)
	if err != nil {
		return cp, err
	}

	if url.User != nil {
		cp.User = url.User.Username()
		cp.Password, _ = url.User.Password()
	}

	parts := strings.SplitN(url.Host, ":", 2)
	cp.Host = parts[0]
	if len(parts) == 2 {
		p, err := strconv.ParseUint(parts[1], 10, 16)
		if err != nil {
			return cp, err
		}
		cp.Port = uint16(p)
	}
	cp.Database = strings.TrimLeft(url.Path, "/")

	err = configSSL(url.Query().Get("sslmode"), &cp)
	if err != nil {
		return cp, err
	}

	ignoreKeys := map[string]struct{}{
		"sslmode": {},
	}

	cp.RuntimeParams = make(map[string]string)

	for k, v := range url.Query() {
		if _, ok := ignoreKeys[k]; ok {
			continue
		}

		cp.RuntimeParams[k] = v[0]
	}
	if cp.Password == "" {
		pgpass(&cp)
	}
	return cp, nil
}

var dsnRegexp = regexp.MustCompile(`([a-zA-Z_]+)=((?:"[^"]+")|(?:[^ ]+))`)

// ParseDSN parses a database DSN (data source name) into a ConnConfig
//
// e.g. ParseDSN("user=username password=password host=1.2.3.4 port=5432 dbname=mydb sslmode=disable")
//
// Any options not used by the connection process are parsed into ConnConfig.RuntimeParams.
//
// e.g. ParseDSN("application_name=pgxtest search_path=admin user=username password=password host=1.2.3.4 dbname=mydb")
//
// ParseDSN tries to match libpq behavior with regard to sslmode. See comments
// for ParseEnvLibpq for more information on the security implications of
// sslmode options.
func ParseDSN(s string) (ConnConfig, error) {
	var cp ConnConfig

	m := dsnRegexp.FindAllStringSubmatch(s, -1)

	var sslmode string

	cp.RuntimeParams = make(map[string]string)

	for _, b := range m {
		switch b[1] {
		case "user":
			cp.User = b[2]
		case "password":
			cp.Password = b[2]
		case "host":
			cp.Host = b[2]
		case "port":
			p, err := strconv.ParseUint(b[2], 10, 16)
			if err != nil {
				return cp, err
			}
			cp.Port = uint16(p)
		case "dbname":
			cp.Database = b[2]
		case "sslmode":
			sslmode = b[2]
		default:
			cp.RuntimeParams[b[1]] = b[2]
		}
	}

	err := configSSL(sslmode, &cp)
	if err != nil {
		return cp, err
	}
	if cp.Password == "" {
		pgpass(&cp)
	}
	return cp, nil
}

// ParseConnectionString parses either a URI or a DSN connection string.
// see ParseURI and ParseDSN for details.
func ParseConnectionString(s string) (ConnConfig, error) {
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		return ParseURI(s)
	}
	return ParseDSN(s)
}

// ParseEnvLibpq parses the environment like libpq does into a ConnConfig
//
// See http://www.postgresql.org/docs/9.4/static/libpq-envars.html for details
// on the meaning of environment variables.
//
// ParseEnvLibpq currently recognizes the following environment variables:
// PGHOST
// PGPORT
// PGDATABASE
// PGUSER
// PGPASSWORD
// PGSSLMODE
// PGAPPNAME
//
// Important TLS Security Notes:
// ParseEnvLibpq tries to match libpq behavior with regard to PGSSLMODE. This
// includes defaulting to "prefer" behavior if no environment variable is set.
//
// See http://www.postgresql.org/docs/9.4/static/libpq-ssl.html#LIBPQ-SSL-PROTECTION
// for details on what level of security each sslmode provides.
//
// "verify-ca" mode currently is treated as "verify-full". e.g. It has stronger
// security guarantees than it would with libpq. Do not rely on this behavior as it
// may be possible to match libpq in the future. If you need full security use
// "verify-full".
//
// Several of the PGSSLMODE options (including the default behavior of "prefer")
// will set UseFallbackTLS to true and FallbackTLSConfig to a disabled or
// weakened TLS mode. This means that if ParseEnvLibpq is used, but TLSConfig is
// later set from a different source that UseFallbackTLS MUST be set false to
// avoid the possibility of falling back to weaker or disabled security.
func ParseEnvLibpq() (ConnConfig, error) {
	var cc ConnConfig

	cc.Host = os.Getenv("PGHOST")

	if pgport := os.Getenv("PGPORT"); pgport != "" {
		if port, err := strconv.ParseUint(pgport, 10, 16); err == nil {
			cc.Port = uint16(port)
		} else {
			return cc, err
		}
	}

	cc.Database = os.Getenv("PGDATABASE")
	cc.User = os.Getenv("PGUSER")
	cc.Password = os.Getenv("PGPASSWORD")

	sslmode := os.Getenv("PGSSLMODE")

	err := configSSL(sslmode, &cc)
	if err != nil {
		return cc, err
	}

	cc.RuntimeParams = make(map[string]string)
	if appname := os.Getenv("PGAPPNAME"); appname != "" {
		cc.RuntimeParams["application_name"] = appname
	}
	if cc.Password == "" {
		pgpass(&cc)
	}
	return cc, nil
}

func configSSL(sslmode string, cc *ConnConfig) error {
	// Match libpq default behavior
	if sslmode == "" {
		sslmode = "prefer"
	}

	switch sslmode {
	case "disable":
	case "allow":
		cc.UseFallbackTLS = true
		cc.FallbackTLSConfig = &tls.Config{InsecureSkipVerify: true}
	case "prefer":
		cc.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		cc.UseFallbackTLS = true
		cc.FallbackTLSConfig = nil
	case "require":
		cc.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	case "verify-ca", "verify-full":
		cc.TLSConfig = &tls.Config{
			ServerName: cc.Host,
		}
	default:
		return errors.New("sslmode is invalid")
	}

	return nil
}

// Prepare creates a prepared statement with name and sql. sql can contain placeholders
// for bound parameters. These placeholders are referenced positional as $1, $2, etc.
//
// Prepare is idempotent; i.e. it is safe to call Prepare multiple times with the same
// name and sql arguments. This allows a code path to Prepare and Query/Exec without
// concern for if the statement has already been prepared.
func (c *Conn) Prepare(name, sql string) (ps *PreparedStatement, err error) {
	return c.PrepareEx(context.Background(), name, sql, nil)
}

// PrepareEx creates a prepared statement with name and sql. sql can contain placeholders
// for bound parameters. These placeholders are referenced positional as $1, $2, etc.
// It defers from Prepare as it allows additional options (such as parameter OIDs) to be passed via struct
//
// PrepareEx is idempotent; i.e. it is safe to call PrepareEx multiple times with the same
// name and sql arguments. This allows a code path to PrepareEx and Query/Exec without
// concern for if the statement has already been prepared.
func (c *Conn) PrepareEx(ctx context.Context, name, sql string, opts *PrepareExOptions) (ps *PreparedStatement, err error) {
	err = c.waitForPreviousCancelQuery(ctx)
	if err != nil {
		return nil, err
	}

	err = c.initContext(ctx)
	if err != nil {
		return nil, err
	}

	ps, err = c.prepareEx(name, sql, opts)
	err = c.termContext(err)
	return ps, err
}

func (c *Conn) prepareEx(name, sql string, opts *PrepareExOptions) (ps *PreparedStatement, err error) {
	if name != "" {
		if ps, ok := c.preparedStatements[name]; ok && ps.SQL == sql {
			return ps, nil
		}
	}

	if err := c.ensureConnectionReadyForQuery(); err != nil {
		return nil, err
	}

	if c.shouldLog(LogLevelError) {
		defer func() {
			if err != nil {
				c.log(LogLevelError, "prepareEx failed", map[string]interface{}{"err": err, "name": name, "sql": sql})
			}
		}()
	}

	if opts == nil {
		opts = &PrepareExOptions{}
	}

	if len(opts.ParameterOIDs) > 65535 {
		return nil, errors.Errorf("Number of PrepareExOptions ParameterOIDs must be between 0 and 65535, received %d", len(opts.ParameterOIDs))
	}

	buf := appendParse(c.wbuf, name, sql, opts.ParameterOIDs)
	buf = appendDescribe(buf, 'S', name)
	buf = appendSync(buf)

	n, err := c.conn.Write(buf)
	if err != nil {
		if fatalWriteErr(n, err) {
			c.die(err)
		}
		return nil, err
	}
	c.pendingReadyForQueryCount++

	ps = &PreparedStatement{Name: name, SQL: sql}

	var softErr error

	for {
		msg, err := c.rxMsg()
		if err != nil {
			return nil, err
		}

		switch msg := msg.(type) {
		case *pgproto3.ParameterDescription:
			ps.ParameterOIDs = c.rxParameterDescription(msg)

			if len(ps.ParameterOIDs) > 65535 && softErr == nil {
				softErr = errors.Errorf("PostgreSQL supports maximum of 65535 parameters, received %d", len(ps.ParameterOIDs))
			}
		case *pgproto3.RowDescription:
			ps.FieldDescriptions = c.rxRowDescription(msg)
			for i := range ps.FieldDescriptions {
				if dt, ok := c.ConnInfo.DataTypeForOID(ps.FieldDescriptions[i].DataType); ok {
					ps.FieldDescriptions[i].DataTypeName = dt.Name
					if _, ok := dt.Value.(pgtype.BinaryDecoder); ok {
						ps.FieldDescriptions[i].FormatCode = BinaryFormatCode
					} else {
						ps.FieldDescriptions[i].FormatCode = TextFormatCode
					}
				} else {
					return nil, errors.Errorf("unknown oid: %d", ps.FieldDescriptions[i].DataType)
				}
			}
		case *pgproto3.ReadyForQuery:
			c.rxReadyForQuery(msg)

			if softErr == nil {
				c.preparedStatements[name] = ps
			}

			return ps, softErr
		default:
			if e := c.processContextFreeMsg(msg); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

// Deallocate released a prepared statement
func (c *Conn) Deallocate(name string) error {
	return c.deallocateContext(context.Background(), name)
}

// TODO - consider making this public
func (c *Conn) deallocateContext(ctx context.Context, name string) (err error) {
	err = c.waitForPreviousCancelQuery(ctx)
	if err != nil {
		return err
	}

	err = c.initContext(ctx)
	if err != nil {
		return err
	}
	defer func() {
		err = c.termContext(err)
	}()

	if err := c.ensureConnectionReadyForQuery(); err != nil {
		return err
	}

	delete(c.preparedStatements, name)

	// close
	buf := c.wbuf
	buf = append(buf, 'C')
	sp := len(buf)
	buf = pgio.AppendInt32(buf, -1)
	buf = append(buf, 'S')
	buf = append(buf, name...)
	buf = append(buf, 0)
	pgio.SetInt32(buf[sp:], int32(len(buf[sp:])))

	// flush
	buf = append(buf, 'H')
	buf = pgio.AppendInt32(buf, 4)

	_, err = c.conn.Write(buf)
	if err != nil {
		c.die(err)
		return err
	}

	for {
		msg, err := c.rxMsg()
		if err != nil {
			return err
		}

		switch msg.(type) {
		case *pgproto3.CloseComplete:
			return nil
		default:
			err = c.processContextFreeMsg(msg)
			if err != nil {
				return err
			}
		}
	}
}

// Listen establishes a PostgreSQL listen/notify to channel
func (c *Conn) Listen(channel string) error {
	_, err := c.Exec("listen " + quoteIdentifier(channel))
	if err != nil {
		return err
	}

	c.channels[channel] = struct{}{}

	return nil
}

// Unlisten unsubscribes from a listen channel
func (c *Conn) Unlisten(channel string) error {
	_, err := c.Exec("unlisten " + quoteIdentifier(channel))
	if err != nil {
		return err
	}

	delete(c.channels, channel)
	return nil
}

// WaitForNotification waits for a PostgreSQL notification.
func (c *Conn) WaitForNotification(ctx context.Context) (notification *Notification, err error) {
	// Return already received notification immediately
	if len(c.notifications) > 0 {
		notification := c.notifications[0]
		c.notifications = c.notifications[1:]
		return notification, nil
	}

	err = c.waitForPreviousCancelQuery(ctx)
	if err != nil {
		return nil, err
	}

	err = c.initContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = c.termContext(err)
	}()

	if err = c.lock(); err != nil {
		return nil, err
	}
	defer func() {
		if unlockErr := c.unlock(); unlockErr != nil && err == nil {
			err = unlockErr
		}
	}()

	if err := c.ensureConnectionReadyForQuery(); err != nil {
		return nil, err
	}

	for {
		msg, err := c.rxMsg()
		if err != nil {
			return nil, err
		}

		err = c.processContextFreeMsg(msg)
		if err != nil {
			return nil, err
		}

		if len(c.notifications) > 0 {
			notification := c.notifications[0]
			c.notifications = c.notifications[1:]
			return notification, nil
		}
	}
}

func (c *Conn) IsAlive() bool {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.status >= connStatusIdle
}

func (c *Conn) CauseOfDeath() error {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.causeOfDeath
}

func (c *Conn) sendQuery(sql string, arguments ...interface{}) (err error) {
	if ps, present := c.preparedStatements[sql]; present {
		return c.sendPreparedQuery(ps, arguments...)
	}
	return c.sendSimpleQuery(sql, arguments...)
}

func (c *Conn) sendSimpleQuery(sql string, args ...interface{}) error {
	if err := c.ensureConnectionReadyForQuery(); err != nil {
		return err
	}

	if len(args) == 0 {
		buf := appendQuery(c.wbuf, sql)

		_, err := c.conn.Write(buf)
		if err != nil {
			c.die(err)
			return err
		}
		c.pendingReadyForQueryCount++

		return nil
	}

	ps, err := c.Prepare("", sql)
	if err != nil {
		return err
	}

	return c.sendPreparedQuery(ps, args...)
}

func (c *Conn) sendPreparedQuery(ps *PreparedStatement, arguments ...interface{}) (err error) {
	if len(ps.ParameterOIDs) != len(arguments) {
		return errors.Errorf("Prepared statement \"%v\" requires %d parameters, but %d were provided", ps.Name, len(ps.ParameterOIDs), len(arguments))
	}

	if err := c.ensureConnectionReadyForQuery(); err != nil {
		return err
	}

	resultFormatCodes := make([]int16, len(ps.FieldDescriptions))
	for i, fd := range ps.FieldDescriptions {
		resultFormatCodes[i] = fd.FormatCode
	}
	buf, err := appendBind(c.wbuf, "", ps.Name, c.ConnInfo, ps.ParameterOIDs, arguments, resultFormatCodes)
	if err != nil {
		return err
	}

	buf = appendExecute(buf, "", 0)
	buf = appendSync(buf)

	n, err := c.conn.Write(buf)
	if err != nil {
		if fatalWriteErr(n, err) {
			c.die(err)
		}
		return err
	}
	c.pendingReadyForQueryCount++

	return nil
}

// fatalWriteError takes the response of a net.Conn.Write and determines if it is fatal
func fatalWriteErr(bytesWritten int, err error) bool {
	// Partial writes break the connection
	if bytesWritten > 0 {
		return true
	}

	netErr, is := err.(net.Error)
	return !(is && netErr.Timeout())
}

// Exec executes sql. sql can be either a prepared statement name or an SQL string.
// arguments should be referenced positionally from the sql string as $1, $2, etc.
func (c *Conn) Exec(sql string, arguments ...interface{}) (commandTag CommandTag, err error) {
	return c.ExecEx(context.Background(), sql, nil, arguments...)
}

// Processes messages that are not exclusive to one context such as
// authentication or query response. The response to these messages is the same
// regardless of when they occur. It also ignores messages that are only
// meaningful in a given context. These messages can occur due to a context
// deadline interrupting message processing. For example, an interrupted query
// may have left DataRow messages on the wire.
func (c *Conn) processContextFreeMsg(msg pgproto3.BackendMessage) (err error) {
	switch msg := msg.(type) {
	case *pgproto3.ErrorResponse:
		return c.rxErrorResponse(msg)
	case *pgproto3.NoticeResponse:
		c.rxNoticeResponse(msg)
	case *pgproto3.NotificationResponse:
		c.rxNotificationResponse(msg)
	case *pgproto3.ReadyForQuery:
		c.rxReadyForQuery(msg)
	case *pgproto3.ParameterStatus:
		c.rxParameterStatus(msg)
	}

	return nil
}

func (c *Conn) rxMsg() (pgproto3.BackendMessage, error) {
	if !c.IsAlive() {
		return nil, ErrDeadConn
	}

	msg, err := c.frontend.Receive()
	if err != nil {
		if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) {
			c.die(err)
		}
		return nil, err
	}

	c.lastActivityTime = time.Now()

	// fmt.Printf("rxMsg: %#v\n", msg)

	return msg, nil
}

func (c *Conn) rxAuthenticationX(msg *pgproto3.Authentication) (err error) {
	switch msg.Type {
	case pgproto3.AuthTypeOk:
	case pgproto3.AuthTypeCleartextPassword:
		err = c.txPasswordMessage(c.config.Password)
	case pgproto3.AuthTypeMD5Password:
		digestedPassword := "md5" + hexMD5(hexMD5(c.config.Password+c.config.User)+string(msg.Salt[:]))
		err = c.txPasswordMessage(digestedPassword)
	default:
		err = errors.New("Received unknown authentication message")
	}

	return
}

func hexMD5(s string) string {
	hash := md5.New()
	io.WriteString(hash, s)
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Conn) rxParameterStatus(msg *pgproto3.ParameterStatus) {
	c.RuntimeParams[msg.Name] = msg.Value
}

func (c *Conn) rxErrorResponse(msg *pgproto3.ErrorResponse) PgError {
	err := PgError{
		Severity:         msg.Severity,
		Code:             msg.Code,
		Message:          msg.Message,
		Detail:           msg.Detail,
		Hint:             msg.Hint,
		Position:         msg.Position,
		InternalPosition: msg.InternalPosition,
		InternalQuery:    msg.InternalQuery,
		Where:            msg.Where,
		SchemaName:       msg.SchemaName,
		TableName:        msg.TableName,
		ColumnName:       msg.ColumnName,
		DataTypeName:     msg.DataTypeName,
		ConstraintName:   msg.ConstraintName,
		File:             msg.File,
		Line:             msg.Line,
		Routine:          msg.Routine,
	}

	if err.Severity == "FATAL" {
		c.die(err)
	}

	return err
}

func (c *Conn) rxNoticeResponse(msg *pgproto3.NoticeResponse) {
	if c.onNotice == nil {
		return
	}

	notice := &Notice{
		Severity:         msg.Severity,
		Code:             msg.Code,
		Message:          msg.Message,
		Detail:           msg.Detail,
		Hint:             msg.Hint,
		Position:         msg.Position,
		InternalPosition: msg.InternalPosition,
		InternalQuery:    msg.InternalQuery,
		Where:            msg.Where,
		SchemaName:       msg.SchemaName,
		TableName:        msg.TableName,
		ColumnName:       msg.ColumnName,
		DataTypeName:     msg.DataTypeName,
		ConstraintName:   msg.ConstraintName,
		File:             msg.File,
		Line:             msg.Line,
		Routine:          msg.Routine,
	}

	c.onNotice(c, notice)
}

func (c *Conn) rxBackendKeyData(msg *pgproto3.BackendKeyData) {
	c.pid = msg.ProcessID
	c.secretKey = msg.SecretKey
}

func (c *Conn) rxReadyForQuery(msg *pgproto3.ReadyForQuery) {
	c.pendingReadyForQueryCount--
	c.txStatus = msg.TxStatus
}

func (c *Conn) rxRowDescription(msg *pgproto3.RowDescription) []FieldDescription {
	fields := make([]FieldDescription, len(msg.Fields))
	for i := 0; i < len(fields); i++ {
		fields[i].Name = msg.Fields[i].Name
		fields[i].Table = pgtype.OID(msg.Fields[i].TableOID)
		fields[i].AttributeNumber = msg.Fields[i].TableAttributeNumber
		fields[i].DataType = pgtype.OID(msg.Fields[i].DataTypeOID)
		fields[i].DataTypeSize = msg.Fields[i].DataTypeSize
		fields[i].Modifier = msg.Fields[i].TypeModifier
		fields[i].FormatCode = msg.Fields[i].Format
	}
	return fields
}

func (c *Conn) rxParameterDescription(msg *pgproto3.ParameterDescription) []pgtype.OID {
	parameters := make([]pgtype.OID, len(msg.ParameterOIDs))
	for i := 0; i < len(parameters); i++ {
		parameters[i] = pgtype.OID(msg.ParameterOIDs[i])
	}
	return parameters
}

func (c *Conn) rxNotificationResponse(msg *pgproto3.NotificationResponse) {
	n := new(Notification)
	n.PID = msg.PID
	n.Channel = msg.Channel
	n.Payload = msg.Payload
	c.notifications = append(c.notifications, n)
}

func (c *Conn) startTLS(tlsConfig *tls.Config) (err error) {
	err = binary.Write(c.conn, binary.BigEndian, []int32{8, 80877103})
	if err != nil {
		return
	}

	response := make([]byte, 1)
	if _, err = io.ReadFull(c.conn, response); err != nil {
		return
	}

	if response[0] != 'S' {
		return ErrTLSRefused
	}

	c.conn = tls.Client(c.conn, tlsConfig)

	return nil
}

func (c *Conn) txPasswordMessage(password string) (err error) {
	buf := c.wbuf
	buf = append(buf, 'p')
	sp := len(buf)
	buf = pgio.AppendInt32(buf, -1)
	buf = append(buf, password...)
	buf = append(buf, 0)
	pgio.SetInt32(buf[sp:], int32(len(buf[sp:])))

	_, err = c.conn.Write(buf)

	return err
}

func (c *Conn) die(err error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.status == connStatusClosed {
		return
	}

	c.status = connStatusClosed
	c.causeOfDeath = err
	c.conn.Close()
}

func (c *Conn) lock() error {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.status != connStatusIdle {
		return ErrConnBusy
	}

	c.status = connStatusBusy
	return nil
}

func (c *Conn) unlock() error {
	c.mux.Lock()
	defer c.mux.Unlock()

	if c.status != connStatusBusy {
		return errors.New("unlock conn that is not busy")
	}

	c.status = connStatusIdle
	return nil
}

func (c *Conn) shouldLog(lvl int) bool {
	return c.logger != nil && c.logLevel >= lvl
}

func (c *Conn) log(lvl LogLevel, msg string, data map[string]interface{}) {
	if data == nil {
		data = map[string]interface{}{}
	}
	if c.pid != 0 {
		data["pid"] = c.pid
	}

	c.logger.Log(lvl, msg, data)
}

// SetLogger replaces the current logger and returns the previous logger.
func (c *Conn) SetLogger(logger Logger) Logger {
	oldLogger := c.logger
	c.logger = logger
	return oldLogger
}

// SetLogLevel replaces the current log level and returns the previous log
// level.
func (c *Conn) SetLogLevel(lvl int) (int, error) {
	oldLvl := c.logLevel

	if lvl < LogLevelNone || lvl > LogLevelTrace {
		return oldLvl, ErrInvalidLogLevel
	}

	c.logLevel = lvl
	return lvl, nil
}

func quoteIdentifier(s string) string {
	return `"` + strings.Replace(s, `"`, `""`, -1) + `"`
}

// cancelQuery sends a cancel request to the PostgreSQL server. It returns an
// error if unable to deliver the cancel request, but lack of an error does not
// ensure that the query was canceled. As specified in the documentation, there
// is no way to be sure a query was canceled. See
// https://www.postgresql.org/docs/current/static/protocol-flow.html#AEN112861
func (c *Conn) cancelQuery() {
	if !atomic.CompareAndSwapInt32(&c.cancelQueryInProgress, 0, 1) {
		panic("cancelQuery when cancelQueryInProgress")
	}

	if err := c.conn.SetDeadline(time.Now()); err != nil {
		c.Close() // Close connection if unable to set deadline
		return
	}

	doCancel := func() error {
		network, address := c.config.networkAddress()
		cancelConn, err := c.config.Dial(network, address)
		if err != nil {
			return err
		}
		defer cancelConn.Close()

		// If server doesn't process cancellation request in bounded time then abort.
		err = cancelConn.SetDeadline(time.Now().Add(15 * time.Second))
		if err != nil {
			return err
		}

		buf := make([]byte, 16)
		binary.BigEndian.PutUint32(buf[0:4], 16)
		binary.BigEndian.PutUint32(buf[4:8], 80877102)
		binary.BigEndian.PutUint32(buf[8:12], uint32(c.pid))
		binary.BigEndian.PutUint32(buf[12:16], uint32(c.secretKey))
		_, err = cancelConn.Write(buf)
		if err != nil {
			return err
		}

		_, err = cancelConn.Read(buf)
		if err != io.EOF {
			return errors.Errorf("Server failed to close connection after cancel query request: %v %v", err, buf)
		}

		return nil
	}

	go func() {
		err := doCancel()
		if err != nil {
			c.Close() // Something is very wrong. Terminate the connection.
		}
		c.cancelQueryCompleted <- struct{}{}
	}()
}

func (c *Conn) Ping(ctx context.Context) error {
	_, err := c.ExecEx(ctx, ";", nil)
	return err
}

func (c *Conn) ExecEx(ctx context.Context, sql string, options *QueryExOptions, arguments ...interface{}) (CommandTag, error) {
	err := c.waitForPreviousCancelQuery(ctx)
	if err != nil {
		return "", err
	}

	if err := c.lock(); err != nil {
		return "", err
	}
	defer c.unlock()

	startTime := time.Now()
	c.lastActivityTime = startTime

	commandTag, err := c.execEx(ctx, sql, options, arguments...)
	if err != nil {
		if c.shouldLog(LogLevelError) {
			c.log(LogLevelError, "Exec", map[string]interface{}{"sql": sql, "args": logQueryArgs(arguments), "err": err})
		}
		return commandTag, err
	}

	if c.shouldLog(LogLevelInfo) {
		endTime := time.Now()
		c.log(LogLevelInfo, "Exec", map[string]interface{}{"sql": sql, "args": logQueryArgs(arguments), "time": endTime.Sub(startTime), "commandTag": commandTag})
	}

	return commandTag, err
}

func (c *Conn) execEx(ctx context.Context, sql string, options *QueryExOptions, arguments ...interface{}) (commandTag CommandTag, err error) {
	err = c.initContext(ctx)
	if err != nil {
		return "", err
	}
	defer func() {
		err = c.termContext(err)
	}()

	if options != nil && options.SimpleProtocol {
		err = c.sanitizeAndSendSimpleQuery(sql, arguments...)
		if err != nil {
			return "", err
		}
	} else if options != nil && len(options.ParameterOIDs) > 0 {
		if err := c.ensureConnectionReadyForQuery(); err != nil {
			return "", err
		}

		buf, err := c.buildOneRoundTripExec(c.wbuf, sql, options, arguments)
		if err != nil {
			return "", err
		}

		buf = appendSync(buf)

		n, err := c.conn.Write(buf)
		if err != nil && fatalWriteErr(n, err) {
			c.die(err)
			return "", err
		}
		c.pendingReadyForQueryCount++
	} else {
		if len(arguments) > 0 {
			ps, ok := c.preparedStatements[sql]
			if !ok {
				var err error
				ps, err = c.prepareEx("", sql, nil)
				if err != nil {
					return "", err
				}
			}

			err = c.sendPreparedQuery(ps, arguments...)
			if err != nil {
				return "", err
			}
		} else {
			if err = c.sendQuery(sql, arguments...); err != nil {
				return
			}
		}
	}

	var softErr error

	for {
		msg, err := c.rxMsg()
		if err != nil {
			return commandTag, err
		}

		switch msg := msg.(type) {
		case *pgproto3.ReadyForQuery:
			c.rxReadyForQuery(msg)
			return commandTag, softErr
		case *pgproto3.CommandComplete:
			commandTag = CommandTag(msg.CommandTag)
		default:
			if e := c.processContextFreeMsg(msg); e != nil && softErr == nil {
				softErr = e
			}
		}
	}
}

func (c *Conn) buildOneRoundTripExec(buf []byte, sql string, options *QueryExOptions, arguments []interface{}) ([]byte, error) {
	if len(arguments) != len(options.ParameterOIDs) {
		return nil, errors.Errorf("mismatched number of arguments (%d) and options.ParameterOIDs (%d)", len(arguments), len(options.ParameterOIDs))
	}

	if len(options.ParameterOIDs) > 65535 {
		return nil, errors.Errorf("Number of QueryExOptions ParameterOIDs must be between 0 and 65535, received %d", len(options.ParameterOIDs))
	}

	buf = appendParse(buf, "", sql, options.ParameterOIDs)
	buf, err := appendBind(buf, "", "", c.ConnInfo, options.ParameterOIDs, arguments, nil)
	if err != nil {
		return nil, err
	}
	buf = appendExecute(buf, "", 0)

	return buf, nil
}

func (c *Conn) initContext(ctx context.Context) error {
	if c.ctxInProgress {
		return errors.New("ctx already in progress")
	}

	if ctx.Done() == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	c.ctxInProgress = true

	go c.contextHandler(ctx)

	return nil
}

func (c *Conn) termContext(opErr error) error {
	if !c.ctxInProgress {
		return opErr
	}

	var err error

	select {
	case err = <-c.closedChan:
		if opErr == nil {
			err = nil
		}
	case c.doneChan <- struct{}{}:
		err = opErr
	}

	c.ctxInProgress = false
	return err
}

func (c *Conn) contextHandler(ctx context.Context) {
	select {
	case <-ctx.Done():
		c.cancelQuery()
		c.closedChan <- ctx.Err()
	case <-c.doneChan:
	}
}

func (c *Conn) waitForPreviousCancelQuery(ctx context.Context) error {
	if atomic.LoadInt32(&c.cancelQueryInProgress) == 0 {
		return nil
	}

	select {
	case <-c.cancelQueryCompleted:
		atomic.StoreInt32(&c.cancelQueryInProgress, 0)
		if err := c.conn.SetDeadline(time.Time{}); err != nil {
			c.Close() // Close connection if unable to disable deadline
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) ensureConnectionReadyForQuery() error {
	for c.pendingReadyForQueryCount > 0 {
		msg, err := c.rxMsg()
		if err != nil {
			return err
		}

		switch msg := msg.(type) {
		case *pgproto3.ErrorResponse:
			pgErr := c.rxErrorResponse(msg)
			if pgErr.Severity == "FATAL" {
				return pgErr
			}
		default:
			err = c.processContextFreeMsg(msg)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
