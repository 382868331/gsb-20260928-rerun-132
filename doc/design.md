# 源码导航服务（第一版）

面向自定义教学语言 EduLang 的跨文件符号查找与引用定位服务。

## 三层分离

- `snapshot`：工程快照。快照不可变，每个文档带版本号；未保存的缓冲区通过
  `open`/`change` 覆盖已保存文件内容，但不改变磁盘版本的可读性。
- `syntax`：语法对象。词法、AST 与所有区间均绑定真实源文字节偏移，
  并提供按区间取回源码文本的能力。
- `symbol`：符号身份。身份是稳定值（SymbolID），与语法对象分开存放；
  局部变量名相同也不会串到外层或其他文件的定义。

## 增量更新

单文件变更只重建该文件的语法与解析结果；索引中记录每个引用所依赖的
全局定义文件。当被依赖文件变化时，只重解析依赖文件，未受影响文件的
符号身份保持稳定。查询结果携带文档版本与快照 ID。

## EduLang 语法（教学语言）

```
prog    := (funcDecl | varDecl)*
varDecl := 'let' IDENT '=' expression
funcDecl:= 'fn' IDENT '(' params ')' block
block   := '{' (varDecl | ifStmt | exprStmt)* '}'
ifStmt  := 'if' expression block ('else' block)?
exprStmt:= expression
```

表达式为标识符、整数、调用与二元运算：`f(a, b+1)`。
作用域为全局 -> 函数 -> 块（含 `if`/`else`，`else` 是新作用域），
内层 `let` 遮蔽外层同名变量；全局函数与全局变量跨文件互相可见。
