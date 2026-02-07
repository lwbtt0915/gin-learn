package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"net/http"
	"os"
	"time"
)

var (
	db              *gorm.DB
	rdb             *redis.Client
	ctx             = context.Background()
	redisExpireTime = 5 * time.Minute
)

type CustomTime time.Time

const timeLayout = "2006-01-02 15:04:05"

// UnmarshalJSON 实现json.Unmarshaler接口，用于Gin自动绑定
func (ct *CustomTime) UnmarshalJSON(data []byte) error {
	// 去除字符串两端的引号
	var timeStr string
	if err := json.Unmarshal(data, &timeStr); err != nil {
		return err
	}

	// 解析时间字符串
	t, err := time.Parse(timeLayout, timeStr)
	if err != nil {
		return fmt.Errorf("时间格式错误，期望格式：%s，实际值：%s", timeLayout, timeStr)
	}

	// 赋值给自定义时间类型
	*ct = CustomTime(t)
	return nil
}

// String 自定义输出格式（可选）
func (ct CustomTime) String() string {
	return time.Time(ct).Format(timeLayout)
}

type User struct {
	ID       int       `gorm:"primary_key" json:"id"`
	Name     string    `gorm:"size:50;not null" json:"name"`
	Email    string    `gorm:"size:100;not null;unique" json:"email"`
	CreateAt time.Time `json:"created_at"`
	UpdateAt time.Time `json:"updated_at"`
}

type UserRequest struct {
	Name     string     `json:"name"`
	Email    string     `json:"email"`
	CreateAt CustomTime `json:"createAt"`
	UpdateAt CustomTime `json:"updateAt"`
}

func initMysql() error {
	dsn := os.Getenv("MYSQL_DSN")
	conn, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("mysql connect failed: %v", err)
	}

	conn.AutoMigrate(&User{})
	db = conn
	return nil
}

func initRedis() error {
	rdb = redis.NewClient(&redis.Options{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       0,
	})

	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("redis connect failed: %v", err)
	}

	return nil
}

func main() {
	err := godotenv.Load()
	if err != nil {
		panic(fmt.Sprintf("load .env failed: %v", err))
	}

	if err := initMysql(); err != nil {
		panic(err)
	}

	if err := initRedis(); err != nil {
		panic(err)
	}

	r := gin.Default()

	api := r.Group("/api/v1/users")
	{
		api.POST("", createUser)       // 创建用户
		api.GET("/:id", getUser)       // 查询用户
		api.PUT("/:id", updateUser)    // 更新用户
		api.DELETE("/:id", deleteUser) // 删除用户
		api.GET("", listUsers)         // 获取用户列表（直接查MySQL）
	}

	// 启动服务
	fmt.Println("server running on http://127.0.0.1:8068")
	r.Run(":8068")
}

// createUser 创建用户（仅写MySQL，不写缓存）
func createUser(c *gin.Context) {
	var req UserRequest

	// 绑定请求体
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var user User
	user.Name = req.Name
	user.Email = req.Email
	user.CreateAt = time.Time(req.CreateAt)
	user.UpdateAt = time.Time(req.UpdateAt)
	// 写入MySQL
	if err := db.Create(&user).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "user created",
		"data":    req,
	})
}

// getUser 获取单个用户（优先查Redis，缓存未命中则查MySQL并写入缓存）
func getUser(c *gin.Context) {
	id := c.Param("id")
	cacheKey := fmt.Sprintf("user:%s", id)

	// 1. 先查Redis缓存
	var user User
	cacheData, err := rdb.Get(ctx, cacheKey).Result()
	if err == nil {
		// 缓存命中：直接返回（这里简化，实际可反序列化JSON）
		// 注：示例中缓存仅存ID，实际项目可存完整JSON字符串
		if err := db.Where("id = ?", id).First(&user).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": user, "source": "redis"})
		return
	}

	fmt.Println(cacheData)

	// 2. 缓存未命中：查MySQL
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}

	// 3. 写入Redis缓存
	if err := rdb.Set(ctx, cacheKey, id, redisExpireTime).Err(); err != nil {
		fmt.Printf("redis set failed: %v\n", err) // 仅打印日志，不影响接口返回
	}

	c.JSON(http.StatusOK, gin.H{"data": user, "source": "mysql"})
}

// updateUser 更新用户（更新MySQL，删除Redis缓存）
func updateUser(c *gin.Context) {
	id := c.Param("id")
	cacheKey := fmt.Sprintf("user:%s", id)

	var req User
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// 更新MySQL
	if err := db.Model(&User{}).Where("id = ?", id).Updates(req).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 删除Redis缓存（避免缓存脏数据）
	if err := rdb.Del(ctx, cacheKey).Err(); err != nil {
		fmt.Printf("redis del failed: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "user updated"})
}

// deleteUser 删除用户（删除MySQL，删除Redis缓存）
func deleteUser(c *gin.Context) {
	id := c.Param("id")
	cacheKey := fmt.Sprintf("user:%s", id)

	// 删除MySQL数据
	if err := db.Where("id = ?", id).Delete(&User{}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 删除Redis缓存
	if err := rdb.Del(ctx, cacheKey).Err(); err != nil {
		fmt.Printf("redis del failed: %v\n", err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "user deleted"})
}

// listUsers 获取用户列表（直接查MySQL，不缓存，避免列表频繁变化）
func listUsers(c *gin.Context) {
	var users []User
	if err := db.Find(&users).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": users, "count": len(users)})
}
