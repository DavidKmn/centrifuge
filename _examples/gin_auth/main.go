package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/centrifugal/centrifuge"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type clientMessage struct {
	Timestamp int64  `json:"timestamp"`
	Input     string `json:"input"`
}

func handleLog(e centrifuge.LogEntry) {
	log.Printf("%s: %v", e.Message, e.Fields)
}

type contextKey int

var ginContextKey contextKey

// GinContextToContextMiddleware - at the resolver level we only have access
// to context.Context inside centrifuge, but we need the gin context. So we
// create a gin middleware to add its context to the context.Context used by
// centrifuge websocket server.
func GinContextToContextMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), ginContextKey, c)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// GinContextFromContext - we recover the gin context from the context.Context
// struct where we added it just above
func GinContextFromContext(ctx context.Context) (*gin.Context, error) {
	ginContext := ctx.Value(ginContextKey)
	if ginContext == nil {
		err := fmt.Errorf("could not retrieve gin.Context")
		return nil, err
	}
	gc, ok := ginContext.(*gin.Context)
	if !ok {
		err := fmt.Errorf("gin.Context has wrong type")
		return nil, err
	}
	return gc, nil
}

// Finally we can use gin context in the auth middleware of centrifuge.
func authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// We get gin ctx from context.Context struct.
		gc, err := GinContextFromContext(ctx)
		if err != nil {
			fmt.Printf("Failed to retrieve gin context")
			fmt.Print(err.Error())
			return
		}
		// And now we can access gin session.
		s := sessions.Default(gc)
		username := s.Get("user").(string)
		if username != "" {
			fmt.Printf("Successful websocket auth for user %s\n", username)
		} else {
			fmt.Printf("Failed websocket auth for user %s\n", username)
			return
		}
		newCtx := centrifuge.SetCredentials(ctx, &centrifuge.Credentials{
			UserID: s.Get("user").(string),
		})
		r = r.WithContext(newCtx)
		h.ServeHTTP(w, r)
	})
}

func main() {
	cfg := centrifuge.DefaultConfig
	cfg.LogLevel = centrifuge.LogLevelDebug
	cfg.LogHandler = handleLog

	node, _ := centrifuge.New(cfg)

	node.OnConnect(func(c *centrifuge.Client) {
		transport := c.Transport()
		log.Printf("user %s connected via %s.", c.UserID(), transport.Name())

		// Connect handler should not block, so start separate goroutine to
		// periodically send messages to client.
		go func() {
			for {
				select {
				case <-c.Context().Done():
					return
				case <-time.After(5 * time.Second):
					err := c.Send([]byte(`{"time": "` + strconv.FormatInt(time.Now().Unix(), 10) + `"}`))
					if err != nil {
						if err == io.EOF {
							return
						}
						log.Println(err.Error())
					}
				}
			}
		}()
	})

	node.OnRefresh(func(c *centrifuge.Client, e centrifuge.RefreshEvent) (centrifuge.RefreshReply, error) {
		log.Printf("user %s connection is going to expire, refreshing", c.UserID())
		return centrifuge.RefreshReply{
			ExpireAt: time.Now().Unix() + 10,
		}, nil
	})

	node.OnSubscribe(func(c *centrifuge.Client, e centrifuge.SubscribeEvent) (centrifuge.SubscribeReply, error) {
		log.Printf("user %s subscribes on %s", c.UserID(), e.Channel)
		return centrifuge.SubscribeReply{}, nil
	})

	node.OnUnsubscribe(func(c *centrifuge.Client, e centrifuge.UnsubscribeEvent) {
		log.Printf("user %s unsubscribed from %s", c.UserID(), e.Channel)
	})

	node.OnPublish(func(c *centrifuge.Client, e centrifuge.PublishEvent) (centrifuge.PublishReply, error) {
		log.Printf("user %s publishes into channel %s: %s", c.UserID(), e.Channel, string(e.Data))
		var msg clientMessage
		err := json.Unmarshal(e.Data, &msg)
		if err != nil {
			return centrifuge.PublishReply{}, centrifuge.ErrorBadRequest
		}
		return centrifuge.PublishReply{}, nil
	})

	node.OnRPC(func(c *centrifuge.Client, e centrifuge.RPCEvent) (centrifuge.RPCReply, error) {
		log.Printf("RPC from user: %s, data: %s", c.UserID(), string(e.Data))
		return centrifuge.RPCReply{
			Data: []byte(`{"year": "2020"}`),
		}, nil
	})

	node.OnMessage(func(c *centrifuge.Client, e centrifuge.MessageEvent) {
		log.Printf("Message from user: %s, data: %s", c.UserID(), string(e.Data))
	})

	node.OnDisconnect(func(c *centrifuge.Client, e centrifuge.DisconnectEvent) {
		log.Printf("user %s disconnected, disconnect: %s", c.UserID(), e.Disconnect)
	})

	// We also start a separate goroutine for centrifuge itself, since we
	// still need to run gin web server.
	go func() {
		if err := node.Run(); err != nil {
			log.Fatal(err)
		}
	}()

	r := gin.Default()
	store := cookie.NewStore([]byte("secret_string"))
	r.Use(sessions.Sessions("session_name", store))
	r.LoadHTMLFiles("./login_form.html", "./chat.html")
	// Here we tell gin to use the middleware we created just above
	r.Use(GinContextToContextMiddleware())

	r.GET("/login", func(c *gin.Context) {
		s := sessions.Default(c)
		if s.Get("user") != nil && s.Get("user").(string) == "email@email.com" {
			c.Redirect(http.StatusMovedPermanently, "/chat")
			c.Abort()
		} else {
			c.HTML(200, "login_form.html", gin.H{})
		}
	})

	r.POST("/login", func(c *gin.Context) {
		email := c.PostForm("email")
		passwd := c.PostForm("password")
		s := sessions.Default(c)
		if email == "email@email.com" && passwd == "password" {
			s.Set("user", email)
			_ = s.Save()
			c.Redirect(http.StatusMovedPermanently, "/chat")
			c.Abort()
		} else {
			c.JSON(403, gin.H{
				"message": "Bad email/password combination",
			})
		}
	})

	r.GET("/connection/websocket", gin.WrapH(authMiddleware(centrifuge.NewWebsocketHandler(node, centrifuge.WebsocketConfig{}))))
	r.GET("/connection/sockjs", gin.WrapH(authMiddleware(centrifuge.NewSockjsHandler(node, centrifuge.SockjsConfig{
		URL:           "https://cdn.jsdelivr.net/npm/sockjs-client@1/dist/sockjs.min.js",
		HandlerPrefix: "/connection/sockjs",
	}))))

	r.GET("/chat", func(c *gin.Context) {
		s := sessions.Default(c)
		if s.Get("user") != nil {
			c.HTML(200, "chat.html", gin.H{})
		} else {
			c.JSON(403, gin.H{
				"message": "Not logged in!",
			})
		}
		c.Abort()
	})

	_ = r.Run() // listen and serve on 0.0.0.0:8080
}
