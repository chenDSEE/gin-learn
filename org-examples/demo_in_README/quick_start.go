package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()

	// 注意，因为 gin.Engine embedded 了 gin.RouterGroup
	// 所以这里是直接采用 gin.RouterGroup.Get 的
	//r.GET("/ping", func(c *gin.Context) {
	//	c.JSON(http.StatusOK, gin.H{
	//		"message": "pong",
	//	})
	//})

	r.GET("/user/info", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "pong-1",
		})
	})

	r.GET("/user/info/data", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "pong-2",
		})
	})


	r.Run("localhost:80") // 设定监听地址，并启动 server 以及相应的监听函数
}