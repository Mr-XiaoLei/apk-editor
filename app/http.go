package main

import (
	"crypto/tls"
	"embed"
	"fmt"
	"log"
	"net"
	"net/http"
)

//go:embed html/dist/*
var embeddedFiles embed.FS

//go:embed pem/cert.pem pem/key.pem
var pemFiles embed.FS

var vwPort string
var staticHandler = http.FileServer(http.FS(embeddedFiles))

func fileHandle(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "html/dist" + r.URL.Path
	_, err := embeddedFiles.Open(r.URL.Path)
	if err != nil {
		//默认返回主页
		r.URL.Path = "html/dist/"
	}
	staticHandler.ServeHTTP(w, r)

}
func getHttpSchema(tls bool) string {
	if tls {
		return "https"
	}
	return "http"
}
func runHttp(tls bool) (*http.Server, *net.TCPAddr, error) {
	http.HandleFunc("/", fileHandle)
	http.HandleFunc("/tool/html2apk", Html2Apk)
	// 创建一个监听器，端口设置为 0，表示由系统分配空闲端口
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, nil, err
	}
	server := &http.Server{TLSConfig: getTlsConf()}
	addr := listener.Addr().(*net.TCPAddr)
	vwPort = fmt.Sprintf("%s://localhost:%d", getHttpSchema(tls), addr.Port)
	fmt.Println(vwPort)
	go func(server *http.Server) {
		if tls {
			server.ServeTLS(listener, "", "")
		} else {
			server.Serve(listener)
		}
	}(server)
	return server, addr, nil
}

func getTlsConf() *tls.Config {
	// 读取嵌入的证书和密钥
	certData, err := pemFiles.ReadFile("pem/cert.pem")
	if err != nil {
		log.Fatalf("无法读取证书文件: %v", err)
	}

	keyData, err := pemFiles.ReadFile("pem/key.pem")
	if err != nil {
		log.Fatalf("无法读取密钥文件: %v", err)
	}

	// 加载证书和密钥
	cert, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		log.Fatalf("加载证书和密钥失败: %v", err)
	}

	// 创建 TLS 配置
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
	}
	return tlsConfig
}
