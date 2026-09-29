// Package symbol 定义与语法对象分离的符号身份。
// SymbolID 是稳定值：同一文件同一容器路径、同名、同槽位的声明
// 在未触及该声明的增量更新之间保持不变。
package symbol

import "fmt"

type Kind int

const (
	GlobalVar Kind = iota
	Func
	Param
	LocalVar
)

func (k Kind) String() string {
	switch k {
	case GlobalVar:
		return "global_var"
	case Func:
		return "func"
	case Param:
		return "param"
	default:
		return "local_var"
	}
}

// ID 是符号身份。Global 对函数与全局变量成立：跨文件按名字唯一定位。
// 局部身份额外携带容器路径与槽位，杜绝同名局部变量串到外层/其他文件。
type ID struct {
	Global bool   `json:"global"`
	Name   string `json:"name"`

	// 局部身份字段（Global == false 时有效）
	File      string `json:"file,omitempty"`
	Container string `json:"container,omitempty"` // 例：greet/if#0/else#0
	Kind      Kind   `json:"kind,omitempty"`
	Slot      int    `json:"slot,omitempty"` // 同容器内同名声明的序号
}

func (id ID) String() string {
	if id.Global {
		return fmt.Sprintf("global:%s", id.Name)
	}
	return fmt.Sprintf("local:%s|%s|%s|%d", id.File, id.Container, id.Name, id.Slot)
}

func GlobalID(name string) ID {
	return ID{Global: true, Name: name}
}

func LocalID(file, container, name string, kind Kind, slot int) ID {
	return ID{File: file, Container: container, Name: name, Kind: kind, Slot: slot}
}

// Symbol 是身份与当前版本语法信息的绑定。
type Symbol struct {
	ID        ID     `json:"id"`
	Kind      Kind   `json:"kind"`
	Name      string `json:"name"`
	File      string `json:"file"`
	DefStart  int    `json:"def_start"`
	DefEnd    int    `json:"def_end"`
	Container string `json:"container"`
}

// Ref 是一次引用记录，Range 为真实源文字节区间。
type Ref struct {
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
	Def   ID     `json:"def"`
	// GlobalName 在全局解析前记录候选全局名字；空串表示局部引用。
	GlobalName string `json:"global_name,omitempty"`
	Resolved   bool   `json:"resolved"`
}

// IsDefinition 报告引用区间是否即声明本身（参数/变量名）。
func (r Ref) IsDefinition() bool {
	return r.Start >= 0 && r.End > r.Start && r.Text != ""
}
