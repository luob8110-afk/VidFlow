package media

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"feedsystem_video_go/internal/middleware/jwt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (s *Service) Register(r *gin.Engine, video *gin.RouterGroup) {
	assets := video.Group("/assets", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
	})
	assets.POST("", s.initHTTP)
	assets.GET("/:id", s.statusHTTP)
	assets.POST("/:id/complete", s.completeHTTP)
	assets.POST("/:id/parts/:part", s.partHTTP)
	assets.DELETE("/:id", s.cancelHTTP)
	r.GET("/media/:id/:file", s.playbackHTTP)
	r.HEAD("/media/:id/:file", s.playbackHTTP)
}

func (s *Service) initHTTP(c *gin.Context) {
	var in InitRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, ErrInvalid)
		return
	}
	ctx, owner, cancel := assetRequest(c)
	defer cancel()
	value, err := s.Init(ctx, owner, in)
	writeView(c, value, err)
}

func (s *Service) statusHTTP(c *gin.Context) {
	ctx, owner, cancel := assetRequest(c)
	defer cancel()
	value, err := s.Status(ctx, owner, c.Param("id"))
	writeView(c, value, err)
}

func (s *Service) completeHTTP(c *gin.Context) {
	ctx, owner, cancel := assetRequest(c)
	defer cancel()
	value, err := s.Complete(ctx, owner, c.Param("id"))
	writeView(c, value, err)
}

func (s *Service) partHTTP(c *gin.Context) {
	n, err := strconv.Atoi(c.Param("part"))
	if err != nil {
		writeError(c, ErrInvalid)
		return
	}
	ctx, owner, cancel := assetRequest(c)
	defer cancel()
	url, err := s.PartURL(ctx, owner, c.Param("id"), n)
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": url})
}

func (s *Service) cancelHTTP(c *gin.Context) {
	ctx, owner, cancel := assetRequest(c)
	defer cancel()
	if err := s.Cancel(ctx, owner, c.Param("id")); err != nil {
		writeError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Service) playbackHTTP(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, name := c.Param("id"), c.Param("file")
	if !validHex(id, 32) || (name != videoOutput && name != coverOutput) {
		c.Status(http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	var a Asset
	if err := s.db.WithContext(ctx).Select("id", "output_token").
		Where("id = ? AND status = ?", id, Published).Take(&a).Error; err != nil {
		writeError(c, err)
		return
	}
	url, err := s.storage.playbackURL(ctx, a.output(a.OutputToken, name), c.Request.Method)
	if err != nil {
		writeError(c, storageError(err))
		return
	}
	c.Redirect(http.StatusTemporaryRedirect, url)
}

func assetRequest(c *gin.Context) (context.Context, uint, context.CancelFunc) {
	owner, _ := jwt.GetAccountID(c) // The enclosing group requires JWT.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	return ctx, owner, cancel
}

func writeView(c *gin.Context, view View, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func WritePublishError(c *gin.Context, err error) { writeError(c, err) }

func writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "媒体操作失败，请重试"
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, message = http.StatusNotFound, "媒体资产不存在"
	case errors.Is(err, ErrExpired):
		status, message = http.StatusGone, ErrExpired.Error()
	case errors.Is(err, ErrConflict), errors.Is(err, ErrIncomplete):
		status, message = http.StatusConflict, err.Error()
	case errors.Is(err, ErrInvalid):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusServiceUnavailable, ErrUnavailable.Error()
	}
	if status >= 500 {
		log.Printf("media %s: %v", c.FullPath(), err)
	}
	c.JSON(status, gin.H{"error": message})
}
