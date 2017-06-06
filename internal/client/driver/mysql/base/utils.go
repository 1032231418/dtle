package base

import (
	gosql "database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	usql "udup/internal/client/driver/mysql/sql"
	umconf "udup/internal/config/mysql"
)

var (
	prettifyDurationRegexp = regexp.MustCompile("([.][0-9]+)")
)

func PrettifyDurationOutput(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	result := fmt.Sprintf("%s", d)
	result = prettifyDurationRegexp.ReplaceAllString(result, "")
	return result
}

func FileExists(fileName string) bool {
	if _, err := os.Stat(fileName); err == nil {
		return true
	}
	return false
}

// StringContainsAll returns true if `s` contains all non empty given `substrings`
// The function returns `false` if no non-empty arguments are given.
func StringContainsAll(s string, substrings ...string) bool {
	nonEmptyStringsFound := false
	for _, substring := range substrings {
		if substring == "" {
			continue
		}
		if strings.Contains(s, substring) {
			nonEmptyStringsFound = true
		} else {
			// Immediate failure
			return false
		}
	}
	return nonEmptyStringsFound
}

const MaxTableNameLength = 64

type ReplicationLagResult struct {
	Key umconf.InstanceKey
	Lag time.Duration
	Err error
}

func NewNoReplicationLagResult() *ReplicationLagResult {
	return &ReplicationLagResult{Lag: 0, Err: nil}
}

func (r *ReplicationLagResult) HasLag() bool {
	return r.Lag > 0
}

// GetReplicationLag returns replication lag for a given connection config; either by explicit query
// or via SHOW SLAVE STATUS
func GetReplicationLag(connectionConfig *umconf.ConnectionConfig) (replicationLag time.Duration, err error) {
	dbUri := connectionConfig.GetDBUri()
	var db *gosql.DB
	if db, _, err = usql.GetDB(dbUri); err != nil {
		return replicationLag, err
	}

	err = usql.QueryRowsMap(db, `show slave status`, func(m usql.RowMap) error {
		slaveIORunning := m.GetString("Slave_IO_Running")
		slaveSQLRunning := m.GetString("Slave_SQL_Running")
		secondsBehindMaster := m.GetNullInt64("Seconds_Behind_Master")
		if !secondsBehindMaster.Valid {
			return fmt.Errorf("replication not running; Slave_IO_Running=%+v, Slave_SQL_Running=%+v", slaveIORunning, slaveSQLRunning)
		}
		replicationLag = time.Duration(secondsBehindMaster.Int64) * time.Second
		return nil
	})
	return replicationLag, err
}

func GetMasterKeyFromSlaveStatus(connectionConfig *umconf.ConnectionConfig) (masterKey *umconf.InstanceKey, err error) {
	currentUri := connectionConfig.GetDBUri()
	db, _, err := usql.GetDB(currentUri)
	if err != nil {
		return nil, err
	}
	err = usql.QueryRowsMap(db, `show slave status`, func(rowMap usql.RowMap) error {
		// We wish to recognize the case where the topology's master actually has replication configuration.
		// This can happen when a DBA issues a `RESET SLAVE` instead of `RESET SLAVE ALL`.

		// An empty log file indicates this is a master:
		if rowMap.GetString("Master_Log_File") == "" {
			return nil
		}

		slaveIORunning := rowMap.GetString("Slave_IO_Running")
		slaveSQLRunning := rowMap.GetString("Slave_SQL_Running")

		//
		if slaveIORunning != "Yes" || slaveSQLRunning != "Yes" {
			return fmt.Errorf("Replication on %+v is broken: Slave_IO_Running: %s, Slave_SQL_Running: %s. Please make sure replication runs before using udup.",
				connectionConfig.Key,
				slaveIORunning,
				slaveSQLRunning,
			)
		}

		masterKey = &umconf.InstanceKey{
			Host: rowMap.GetString("Master_Host"),
			Port: rowMap.GetInt("Master_Port"),
		}
		return nil
	})

	return masterKey, err
}

func GetMasterConnectionConfigSafe(connectionConfig *umconf.ConnectionConfig, visitedKeys *umconf.InstanceKeyMap, allowMasterMaster bool) (masterConfig *umconf.ConnectionConfig, err error) {
	//log.Debugf("Looking for master on %+v", connectionConfig.Key)

	masterKey, err := GetMasterKeyFromSlaveStatus(connectionConfig)
	if err != nil {
		return nil, err
	}
	if masterKey == nil {
		return connectionConfig, nil
	}
	if !masterKey.IsValid() {
		return connectionConfig, nil
	}
	masterConfig = connectionConfig.Duplicate()
	masterConfig.Key = *masterKey

	//log.Debugf("Master of %+v is %+v", connectionConfig.Key, masterConfig.Key)
	if visitedKeys.HasKey(masterConfig.Key) {
		if allowMasterMaster {
			return connectionConfig, nil
		}
		return nil, fmt.Errorf("There seems to be a master-master setup at %+v. This is unsupported. Bailing out", masterConfig.Key)
	}
	visitedKeys.AddKey(masterConfig.Key)
	return GetMasterConnectionConfigSafe(masterConfig, visitedKeys, allowMasterMaster)
}

func GetReplicationBinlogCoordinates(db *gosql.DB) (readBinlogCoordinates *BinlogCoordinates, executeBinlogCoordinates *BinlogCoordinates, err error) {
	err = usql.QueryRowsMap(db, `show slave status`, func(m usql.RowMap) error {
		readBinlogCoordinates = &BinlogCoordinates{
			LogFile: m.GetString("Master_Log_File"),
			LogPos:  m.GetInt64("Read_Master_Log_Pos"),
		}
		executeBinlogCoordinates = &BinlogCoordinates{
			LogFile: m.GetString("Relay_Master_Log_File"),
			LogPos:  m.GetInt64("Exec_Master_Log_Pos"),
		}
		return nil
	})
	return readBinlogCoordinates, executeBinlogCoordinates, err
}

func GetSelfBinlogCoordinates(db *gosql.DB) (selfBinlogCoordinates *BinlogCoordinates, err error) {
	err = usql.QueryRowsMap(db, `show master status`, func(m usql.RowMap) error {
		selfBinlogCoordinates = &BinlogCoordinates{
			LogFile: m.GetString("File"),
			LogPos:  m.GetInt64("Position"),
		}
		return nil
	})
	return selfBinlogCoordinates, err
}

// GetTableColumns reads column list from given table
func GetTableColumns(db *gosql.DB, databaseName, tableName string) (*umconf.ColumnList, error) {
	query := fmt.Sprintf(`
		show columns from %s.%s
		`,
		usql.EscapeName(databaseName),
		usql.EscapeName(tableName),
	)
	columnNames := []string{}
	err := usql.QueryRowsMap(db, query, func(rowMap usql.RowMap) error {
		columnNames = append(columnNames, rowMap.GetString("Field"))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(columnNames) == 0 {
		return nil, fmt.Errorf("Found 0 columns on %s.%s. Bailing out",
			usql.EscapeName(databaseName),
			usql.EscapeName(tableName),
		)
	}
	return umconf.NewColumnList(columnNames), nil
}
