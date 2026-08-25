#!/bin/bash

# 检查是否提供了参数
if [ -z "$1" ]; then
    echo "用法: $0 <输出文件名>"
    exit 1
fi

# 执行交叉编译
GOOS=linux GOARCH=amd64 go build -o "$1" main.go

# 检查编译是否成功
if [ $? -eq 0 ]; then
    echo "编译成功，输出文件: $1"
else
    echo "编译失败"
    exit 1
fi