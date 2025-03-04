package river

import (
	"fmt"
	"strings"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/juju/errors"
)

type MySQLBulkRequest struct {
	Action string        // INSERT/UPDATE/DELETE
	Table  string        // 目标表名
	Values []interface{} // 值
	Query  string        // SQL语句
}

type customEventHandler struct {
	eventHandler
}

func (h *customEventHandler) OnRow(e *canal.RowsEvent) error {
	rule, ok := h.r.rules[ruleKey(e.Table.Schema, e.Table.Name)]
	if !ok {
		return nil
	}

	var reqs []*MySQLBulkRequest
	var err error
	switch e.Action {
	case canal.InsertAction:
		reqs, err = h.r.makeInsertSQL(rule, e.Rows)
	case canal.DeleteAction:
		reqs, err = h.r.makeDeleteSQL(rule, e.Rows)
	case canal.UpdateAction:
		reqs, err = h.r.makeUpdateSQL(rule, e.Rows)
	default:
		err = errors.Errorf("invalid rows action %s", e.Action)
	}

	if err != nil {
		h.r.cancel()
		return errors.Errorf("make %s MySQL request err %v, close sync", e.Action, err)
	}

	h.r.syncCh <- reqs

	return h.r.ctx.Err()
}

// 获取表的列名
func (r *River) getTableColumns(rule *Rule) []string {
	columns := make([]string, 0, len(rule.TableInfo.Columns))
	for _, column := range rule.TableInfo.Columns {
		if !rule.CheckFilter(column.Name) {
			continue
		}
		columns = append(columns, column.Name)
	}
	return columns
}

func (r *River) makeInsertSQL(rule *Rule, rows [][]interface{}) ([]*MySQLBulkRequest, error) {
	reqs := make([]*MySQLBulkRequest, 0, len(rows))

	columns := r.getTableColumns(rule)
	if len(columns) == 0 {
		return nil, errors.New("no columns found")
	}

	placeholder := "(" + strings.Join(createPlaceholders(len(columns)), ",") + ")"
	query := fmt.Sprintf("INSERT INTO %s.%s (%s) VALUES %s",
		rule.TargetSchema,
		rule.TargetTable,
		strings.Join(columns, ","),
		placeholder)

	for _, row := range rows {
		values := make([]interface{}, 0, len(columns))
		// TODO columns 顺序与 row 中内容顺序是否一一对应
		for i, col := range rule.TableInfo.Columns {
			if rule.CheckFilter(col.Name) {
				values = append(values, row[i])
			}
		}

		req := &MySQLBulkRequest{
			Action: "INSERT",
			Table:  rule.TargetTable,
			Values: values,
			Query:  query,
		}
		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) makeUpdateSQL(rule *Rule, rows [][]interface{}) ([]*MySQLBulkRequest, error) {
	if len(rows)%2 == 1 {
		return nil, errors.Errorf("invalid update rows event, must have 2x rows, but %d", len(rows))
	}
	reqs := make([]*MySQLBulkRequest, 0, len(rows)/2)

	columns := r.getTableColumns(rule)
	if len(columns) == 0 {
		return nil, errors.New("no columns found")
	}

	// 获取主键列
	pkColumns := rule.TableInfo.PKColumns
	if len(pkColumns) == 0 {
		return nil, errors.New("no primary key found")
	}

	// 构建 SET 部分，排除主键列
	setParts := make([]string, 0, len(columns))
	for _, col := range columns {
		// 检查是否是主键列
		isPK := false
		for _, pk := range pkColumns {
			if col == rule.TableInfo.Columns[pk].Name {
				isPK = true
				break
			}
		}
		if !isPK {
			setParts = append(setParts, fmt.Sprintf("%s = ?", col))
		}
	}

	whereParts := make([]string, 0, len(pkColumns))
	for _, pk := range pkColumns {
		whereParts = append(whereParts, fmt.Sprintf("%s = ?", rule.TableInfo.Columns[pk].Name))
	}

	query := fmt.Sprintf("UPDATE %s.%s SET %s WHERE %s",
		rule.TargetSchema,
		rule.TargetTable,
		strings.Join(setParts, ", "),
		strings.Join(whereParts, " AND "))

	for i := 0; i < len(rows); i += 2 {
		oldData := rows[i]
		newData := rows[i+1]

		// 构建更新值（非主键的值）和 WHERE 条件的值
		values := make([]interface{}, 0, len(columns))

		// 添加 SET 的值
		for j, col := range rule.TableInfo.Columns {
			if !rule.CheckFilter(col.Name) {
				continue
			}
			// 检查是否是主键列
			isPK := false
			for _, pk := range pkColumns {
				if j == pk {
					isPK = true
					break
				}
			}
			if !isPK {
				values = append(values, newData[j])
			}
		}

		// 添加 WHERE 条件的值（主键的值）
		for _, pk := range pkColumns {
			values = append(values, oldData[pk])
		}

		req := &MySQLBulkRequest{
			Action: "UPDATE",
			Table:  rule.TargetTable,
			Values: values,
			Query:  query,
		}
		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) makeDeleteSQL(rule *Rule, rows [][]interface{}) ([]*MySQLBulkRequest, error) {
	reqs := make([]*MySQLBulkRequest, 0, len(rows))

	// 获取主键列
	pkColumns := rule.TableInfo.PKColumns
	if len(pkColumns) == 0 {
		return nil, errors.New("no primary key found")
	}

	// 构建 WHERE 条件
	whereParts := make([]string, 0, len(pkColumns))
	for _, pk := range pkColumns {
		whereParts = append(whereParts, fmt.Sprintf("%s = ?", rule.TableInfo.Columns[pk].Name))
	}

	// 在 SQL 中直接指定数据库名
	query := fmt.Sprintf("DELETE FROM %s.%s WHERE %s",
		rule.TargetSchema, // 使用目标数据库名
		rule.TargetTable,
		strings.Join(whereParts, " AND "))

	for _, row := range rows {
		// 获取主键值
		values := make([]interface{}, 0, len(pkColumns))
		for _, pk := range pkColumns {
			values = append(values, row[pk])
		}

		req := &MySQLBulkRequest{
			Action: "DELETE",
			Table:  rule.TargetTable,
			Values: values,
			Query:  query,
		}
		reqs = append(reqs, req)
	}

	return reqs, nil
}

func (r *River) executeBatch(reqs []*MySQLBulkRequest) error {
	if len(reqs) == 0 {
		return nil
	}

	// 开启事务
	tx, err := r.targetDB.Begin()
	if err != nil {
		return errors.Trace(err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	groups := groupRequests(reqs)

	for _, group := range groups {
		err = func() error {
			stmt, err := tx.Prepare(group[0].Query)
			if err != nil {
				return errors.Trace(err)
			}
			defer stmt.Close()

			for _, req := range group {
				_, err = stmt.Exec(req.Values...)
				if err != nil {
					return errors.Trace(err)
				}
			}
			return nil
		}()

		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// 将请求按表、操作类型和SQL语句分组
func groupRequests(reqs []*MySQLBulkRequest) [][]*MySQLBulkRequest {
	groups := make(map[string][]*MySQLBulkRequest)

	for _, req := range reqs {
		// 使用表名、操作类型和SQL语句作为key，确保相同SQL语句的请求分在一组
		key := fmt.Sprintf("%s_%s_%s", req.Table, req.Action, req.Query)
		groups[key] = append(groups[key], req)
	}

	result := make([][]*MySQLBulkRequest, 0, len(groups))
	for _, group := range groups {
		result = append(result, group)
	}

	return result
}

// 创建SQL占位符
func createPlaceholders(count int) []string {
	placeholders := make([]string, count)
	for i := 0; i < count; i++ {
		placeholders[i] = "?"
	}
	return placeholders
}
