// navd 是源码导航服务第一版：跨文件符号查找与引用定位。
package main

import (
	"flag"
	"log"
	"net/http"

	"nav/server"
	"nav/service"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	svc := service.New()
	h := server.New(svc).Handler()
	log.Printf("navd listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, h))
}
