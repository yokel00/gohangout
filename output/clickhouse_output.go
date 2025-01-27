package output

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go"
	"github.com/childe/gohangout/topology"
	"github.com/golang/glog"
)

const (
	CLICKHOUSE_DEFAULT_BULK_ACTIONS   = 1000
	CLICKHOUSE_DEFAULT_FLUSH_INTERVAL = 30
)

type ClickhouseOutput struct {
	config map[interface{}]interface{}

	bulk_actions int
	hosts        []string
	fields       []string
	table        string
	username     string
	password     string
	logTopic     string

	fieldsLength int
	query        string
	desc         map[string]*rowDesc
	defaultValue map[string]interface{} // columnName -> defaultValue

	bulkChan   chan []map[string]interface{}
	concurrent int

	events       []map[string]interface{}
	execution_id uint64

	dbSelector HostSelector

	mux       sync.Mutex
	wg        sync.WaitGroup
	closeChan chan bool
}

type rowDesc struct {
	Name              string `json:"name"`
	Type              string `json:"type"`
	DefaultType       string `json:"default_type"`
	DefaultExpression string `json:"default_expression"`
}

func (c *ClickhouseOutput) setTableDesc() {
	c.desc = make(map[string]*rowDesc)

	query := fmt.Sprintf("desc table %s", c.table)
	glog.V(5).Info(query)

	for i := 0; i < c.dbSelector.Size(); i++ {
		nextdb := c.dbSelector.Next()

		db := nextdb.(*sql.DB)

		rows, err := db.Query(query)
		if err != nil {
			glog.Errorf("query %q error: %s", query, err)
			continue
		}
		defer rows.Close()

		columns, err := rows.Columns()
		if err != nil {
			glog.Fatalf("could not get columns from query `%s`: %s", query, err)
		}
		glog.V(10).Infof("desc table columns: %v", columns)

		descMap := make(map[string]string)
		for _, c := range columns {
			descMap[c] = ""
		}

		for rows.Next() {
			values := make([]interface{}, 0)
			for range columns {
				var a string
				values = append(values, &a)
			}

			if err := rows.Scan(values...); err != nil {
				glog.Fatalf("scan rows error: %s", err)
			}

			descMap := make(map[string]string)
			for i, c := range columns {
				value := *values[i].(*string)
				if c == "type" {
					// 特殊处理枚举类型
					if strings.HasPrefix(value, "Enum16") {
						value = "Enum16"
					} else if strings.HasPrefix(value, "Enum8") {
						value = "Enum8"
					}
				}
				descMap[c] = value
			}

			b, err := json.Marshal(descMap)
			if err != nil {
				glog.Fatalf("marshal desc error: %s", err)
			}

			rowDesc := rowDesc{}
			err = json.Unmarshal(b, &rowDesc)
			if err != nil {
				glog.Fatalf("marshal desc error: %s", err)
			}

			glog.V(5).Infof("row desc: %#v", rowDesc)

			c.desc[rowDesc.Name] = &rowDesc
		}

		return
	}
}

// TODO only string, number and ip DEFAULT expression is supported for now
func (c *ClickhouseOutput) setColumnDefault() {
	c.setTableDesc()

	c.defaultValue = make(map[string]interface{})

	var defaultValue *string

	for columnName, d := range c.desc {
		switch d.DefaultType {
		case "DEFAULT":
			defaultValue = &(d.DefaultExpression)
		case "MATERIALIZED":
			glog.Fatal("parse default value: MATERIALIZED expression not supported")
		case "ALIAS":
			glog.Fatal("parse default value: ALIAS expression not supported")
		case "":
			defaultValue = nil
		default:
			glog.Fatal("parse default value: only DEFAULT expression supported")
		}

		switch d.Type {
		case "String", "LowCardinality(String)":
			if defaultValue == nil {
				c.defaultValue[columnName] = ""
			} else {
				c.defaultValue[columnName] = *defaultValue
			}
		case "Date", "DateTime", "DateTime64":
			c.defaultValue[columnName] = time.Unix(0, 0)
		case "UInt8", "UInt16", "UInt32", "UInt64", "Int8", "Int16", "Int32", "Int64":
			if defaultValue == nil {
				c.defaultValue[columnName] = 0
			} else {
				i, e := strconv.ParseInt(*defaultValue, 10, 64)
				if e == nil {
					c.defaultValue[columnName] = i
				} else {
					glog.Fatalf("parse default value `%v` error: %v", defaultValue, e)
				}
			}
		case "Float32", "Float64":
			if defaultValue == nil {
				c.defaultValue[columnName] = float32(0)
			} else {
				i, e := strconv.ParseFloat(*defaultValue, 64)
				if e == nil {
					c.defaultValue[columnName] = i
				} else {
					glog.Fatalf("parse default value `%v` error: %v", defaultValue, e)
				}
			}
		case "IPv4":
			c.defaultValue[columnName] = "0.0.0.0"
		case "IPv6":
			c.defaultValue[columnName] = "::"
		case "Array(String)", "Array(IPv4)", "Array(IPv6)", "Array(Date)", "Array(DateTime)":
			c.defaultValue[columnName] = clickhouse.Array([]string{})
		case "Array(UInt8)":
			c.defaultValue[columnName] = clickhouse.Array([]uint8{})
		case "Array(UInt16)":
			c.defaultValue[columnName] = clickhouse.Array([]uint16{})
		case "Array(UInt32)":
			c.defaultValue[columnName] = clickhouse.Array([]uint32{})
		case "Array(UInt64)":
			c.defaultValue[columnName] = clickhouse.Array([]uint64{})
		case "Array(Int8)":
			c.defaultValue[columnName] = clickhouse.Array([]int8{})
		case "Array(Int16)":
			c.defaultValue[columnName] = clickhouse.Array([]int16{})
		case "Array(Int32)":
			c.defaultValue[columnName] = clickhouse.Array([]int32{})
		case "Array(Int64)":
			c.defaultValue[columnName] = clickhouse.Array([]int64{})
		case "Array(Float32)":
			c.defaultValue[columnName] = clickhouse.Array([]float32{})
		case "Array(Float64)":
			c.defaultValue[columnName] = clickhouse.Array([]float64{})
		case "Enum16":
			// 需要要求列声明的最小枚举值为 ''
			c.defaultValue[columnName] = ""
		case "Enum8":
			// 需要要求列声明的最小枚举值为 ''
			c.defaultValue[columnName] = ""
		default:
			glog.Errorf("column: %s, type: %s. unsupported column type, ignore.", columnName, d.Type)
			continue
		}
	}
}

func (c *ClickhouseOutput) getDatabase() string {
	dbAndTable := strings.Split(c.table, ".")
	dbName := "default"
	if len(dbAndTable) == 2 {
		dbName = dbAndTable[0]
	}
	return dbName
}

func init() {
	Register("Clickhouse", newClickhouseOutput)
}

func newClickhouseOutput(config map[interface{}]interface{}) topology.Output {
	rand.Seed(time.Now().UnixNano())
	p := &ClickhouseOutput{
		config: config,
	}

	if v, ok := config["table"]; ok {
		p.table = v.(string)
	} else {
		glog.Fatalf("table must be set in clickhouse output")
	}

	if v, ok := config["hosts"]; ok {
		for _, h := range v.([]interface{}) {
			p.hosts = append(p.hosts, h.(string))
		}
	} else {
		glog.Fatalf("hosts must be set in clickhouse output")
	}

	if v, ok := config["username"]; ok {
		p.username = v.(string)
	}

	if v, ok := config["password"]; ok {
		p.password = v.(string)
	}

	debug := false
	if v, ok := config["debug"]; ok {
		debug = v.(bool)
	}
	/* 22.3.7支持kafka消息频道
	 */
	if v, ok := config["log_topic"]; ok {
		p.logTopic = v.(string)
	} else {
		glog.Fatalf("kafka_topic must be set in clickhouse output")
	}

	/* 2022.3.2注释：支持动态字段
	if v, ok := config["fields"]; ok {
		for _, f := range v.([]interface{}) {
			p.fields = append(p.fields, f.(string))
		}
	} else {
		glog.Fatalf("fields must be set in clickhouse output")
	}
	if len(p.fields) <= 0 {
		glog.Fatalf("fields length must be > 0")
	}
	p.fieldsLength = len(p.fields)

	fields := make([]string, p.fieldsLength)
	for i := range fields {
		fields[i] = fmt.Sprintf(`"%s"`, p.fields[i])
	}
	questionMarks := make([]string, p.fieldsLength)
	for i := 0; i < p.fieldsLength; i++ {
		questionMarks[i] = "?"
	}
	p.query = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", p.table, strings.Join(fields, ","), strings.Join(questionMarks, ","))
	glog.V(5).Infof("query: %s", p.query)
	*/
	connMaxLifetime := 0
	if v, ok := config["conn_max_life_time"]; ok {
		connMaxLifetime = v.(int)
	}

	dbs := make([]*sql.DB, 0)

	for _, host := range p.hosts {
		dataSourceName := fmt.Sprintf("%s?database=%s&username=%s&password=%s&debug=%v", host, p.getDatabase(), p.username, p.password, debug)
		if db, err := sql.Open("clickhouse", dataSourceName); err == nil {
			if err := db.Ping(); err != nil {
				if exception, ok := err.(*clickhouse.Exception); ok {
					glog.Errorf("[%d] %s \n%s\n", exception.Code, exception.Message, exception.StackTrace)
				} else {
					glog.Errorf("clickhouse ping error: %s", err)
				}
			} else {
				db.SetConnMaxLifetime(time.Second * time.Duration(connMaxLifetime))
				dbs = append(dbs, db)
			}
		} else {
			glog.Errorf("open %s error: %s", host, err)
		}
	}

	glog.V(5).Infof("%d available clickhouse hosts", len(dbs))
	if len(dbs) == 0 {
		glog.Fatal("no available host")
	}

	dbsI := make([]interface{}, len(dbs))
	for i, h := range dbs {
		dbsI[i] = h
	}
	p.dbSelector = NewRRHostSelector(dbsI, 3)

	p.setColumnDefault()
	/* 2022.3.2 新增：支持动态字段
	line:[334,348]
	*/
	p.fieldsLength = len(p.desc)
	fields := make([]string, p.fieldsLength)
	p.fields = make([]string, p.fieldsLength)
	k := 0
	for columnName := range p.desc {
		fields[k] = fmt.Sprintf(`"%s"`, columnName)
		p.fields[k] = columnName
		k++
	}
	questionMarks := make([]string, p.fieldsLength)
	for i := 0; i < p.fieldsLength; i++ {
		questionMarks[i] = "?"
	}
	p.query = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", p.table, strings.Join(fields, ","), strings.Join(questionMarks, ","))
	glog.V(5).Infof("query: %s", p.query)
	//
	concurrent := 1
	if v, ok := config["concurrent"]; ok {
		concurrent = v.(int)
	}
	p.concurrent = concurrent
	p.closeChan = make(chan bool, concurrent)

	p.bulkChan = make(chan []map[string]interface{}, concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			p.wg.Add(1)
			for {
				select {
				case events := <-p.bulkChan:
					p.innerFlush(events)
				case <-p.closeChan:
					p.wg.Done()
					return
				}
			}
		}()
	}

	if v, ok := config["bulk_actions"]; ok {
		p.bulk_actions = v.(int)
	} else {
		p.bulk_actions = CLICKHOUSE_DEFAULT_BULK_ACTIONS
	}

	var flush_interval int
	if v, ok := config["flush_interval"]; ok {
		flush_interval = v.(int)
	} else {
		flush_interval = CLICKHOUSE_DEFAULT_FLUSH_INTERVAL
	}
	go func() {
		for range time.NewTicker(time.Second * time.Duration(flush_interval)).C {
			p.flush()
		}
	}()

	return p
}

func (c *ClickhouseOutput) innerFlush(events []map[string]interface{}) {
	execution_id := atomic.AddUint64(&c.execution_id, 1)
	glog.Infof("write %d docs to clickhouse with execution_id %d", len(events), execution_id)

	for {
		nextdb := c.dbSelector.Next()

		/*** not ReduceWeight for now , so this should not happen
		if nextdb == nil {
			glog.Info("no available db, wait for 30s")
			time.Sleep(30 * time.Second)
			continue
		}
		****/

		tx, err := nextdb.(*sql.DB).Begin()
		if err != nil {
			glog.Errorf("db begin to create transaction error: %s", err)
			continue
		}
		defer tx.Rollback()

		stmt, err := tx.Prepare(c.query)
		if err != nil {
			glog.Errorf("transaction prepare statement error: %s", err)
			return
		}
		defer stmt.Close()

		for _, event := range events {
			/*22.3.7 过滤 kafka_topic
			 */
			//if topic, ok := event["log_topic"]; ok && topic == c.logTopic {
			args := make([]interface{}, c.fieldsLength)
			for i, field := range c.fields {
				if v1, ok := event[field]; ok && v1 != nil {
					/* 2022.3.2 动态字段，类型转换
					注释: line 431
					新增：line [432,438]
					*/
					//args[i] = v
					ct := c.desc[field]
					v2, err := convertCkType(ct.Type, v1)
					if err == nil {
						args[i] = v2
					} else {
						args[i] = v1
					}
				} else {
					if v3, ok := c.defaultValue[field]; ok {
						args[i] = v3
					} else { // this should not happen
						args[i] = ""
					}
				}
			}
			if _, err := stmt.Exec(args...); err != nil {
				glog.Errorf("exec clickhouse insert %v error: %s", event, err)
				return
			}
			//}
		}

		if err := tx.Commit(); err != nil {
			glog.Errorf("exec clickhouse commit error: %s", err)
			return
		}
		glog.Infof("%d docs has been committed to clickhouse", len(events))
		return
	}
}

func (c *ClickhouseOutput) flush() {
	c.mux.Lock()
	if len(c.events) > 0 {
		events := c.events
		c.events = make([]map[string]interface{}, 0, c.bulk_actions)
		c.bulkChan <- events
	}
	c.mux.Unlock()
}

// Emit appends event to c.events, and push to bulkChan if needed
func (c *ClickhouseOutput) Emit(event map[string]interface{}) {
	c.mux.Lock()
	c.events = append(c.events, event)
	if len(c.events) < c.bulk_actions {
		c.mux.Unlock()
		return
	}

	events := c.events
	c.events = make([]map[string]interface{}, 0, c.bulk_actions)
	c.mux.Unlock()

	c.bulkChan <- events
}

func (c *ClickhouseOutput) awaitclose(timeout time.Duration) {
	exit := make(chan bool)
	defer func() {
		select {
		case <-exit:
			glog.Info("all clickhouse flush job done. return")
			return
		case <-time.After(timeout):
			glog.Info("clickhouse await timeout. return")
			return
		}
	}()

	defer func() {
		go func() {
			c.wg.Wait()
			exit <- true
		}()
	}()

	glog.Info("try to write remaining docs to clickhouse")

	c.mux.Lock()
	if len(c.events) <= 0 {
		glog.Info("no docs remain, return")
		c.mux.Unlock()
	} else {
		events := c.events
		c.events = make([]map[string]interface{}, 0, c.bulk_actions)
		c.mux.Unlock()

		glog.Infof("ramain %d docs, write them to clickhouse", len(events))
		c.wg.Add(1)
		go func() {
			c.innerFlush(events)
			c.wg.Done()
		}()
	}

	glog.Info("check if there are events blocking in bulk channel")

	for {
		select {
		case events := <-c.bulkChan:
			c.wg.Add(1)
			go func() {
				c.innerFlush(events)
				c.wg.Done()
			}()
		default:
			return
		}
	}
}

// Shutdown would stop receiving message and emiting
func (c *ClickhouseOutput) Shutdown() {
	for i := 0; i < c.concurrent; i++ {
		c.closeChan <- true
	}
	c.awaitclose(30 * time.Second)
}

/* 2022.3.2 新增
ck数据类型转换
*/
func convertCkType(ckType string, val interface{}) (out interface{}, err error) {
	switch ckType {
	case "Int32":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseInt(val.(json.Number).String(), 10, 32)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseInt(val.(string), 10, 32)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "UInt32":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseUint(val.(json.Number).String(), 10, 32)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseUint(val.(string), 10, 32)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "Int16":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseInt(val.(json.Number).String(), 10, 16)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseInt(val.(string), 10, 16)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "UInt16":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseUint(val.(json.Number).String(), 10, 16)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseUint(val.(string), 10, 16)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "Float32", "Decimal32":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseFloat(val.(json.Number).String(), 32)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseFloat(val.(string), 32)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return float32(1), nil
				}
				return float32(0), nil
			}
		}

	case "Int8":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseInt(val.(json.Number).String(), 10, 8)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseInt(val.(string), 10, 8)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "UInt8":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseUint(val.(json.Number).String(), 10, 8)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseUint(val.(string), 10, 8)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "Int64":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseInt(val.(json.Number).String(), 10, 64)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseInt(val.(string), 10, 64)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}
	case "UInt64":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseUint(val.(json.Number).String(), 10, 64)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseUint(val.(string), 10, 64)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "Float64", "Decimal64":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseFloat(val.(json.Number).String(), 64)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseFloat(val.(string), 64)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return float64(1), nil
				}
				return float64(0), nil
			}
		}

	case "Int256":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseInt(val.(json.Number).String(), 10, 256)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseInt(val.(string), 10, 256)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}

	case "UInt256":
		{
			switch val.(type) {
			case json.Number:
				out, err = strconv.ParseUint(val.(json.Number).String(), 10, 256)
				if err == nil {
					return out, nil
				}
			case string:
				out, err = strconv.ParseUint(val.(string), 10, 256)
				if err == nil {
					return out, nil
				}
			case bool:
				if val.(bool) {
					return 1, nil
				}
				return 0, nil
			}
		}
	}

	if err != nil {
		glog.Fatalf("convertCkType parse default value `%v` error: %v", val, err)
	}
	return val, nil
}
