package biz

import (
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "moredoc/api/v1"
	"moredoc/middleware/auth"
	"moredoc/model"
	"moredoc/util"
	"moredoc/util/filetil"

	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type ginResponse struct {
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
	Code    int         `json:"code,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type AttachmentAPIService struct {
	pb.UnimplementedAttachmentAPIServer
	dbModel *model.DBModel
	logger  *zap.Logger
}

func NewAttachmentAPIService(dbModel *model.DBModel, logger *zap.Logger) (service *AttachmentAPIService) {
	return &AttachmentAPIService{dbModel: dbModel, logger: logger.Named("AttachmentAPIService")}
}

// checkPermission 检查用户权限
func (s *AttachmentAPIService) checkPermission(ctx context.Context) (userClaims *auth.UserClaims, err error) {
	return checkGRPCPermission(s.dbModel, ctx)
}

// checkPermission 检查用户权限
// 文件等的上传，也要验证用户是否有权限，无论是否是管理员
func (s *AttachmentAPIService) checkGinPermission(ctx *gin.Context) (userClaims *auth.UserClaims, statusCode int, err error) {
	return checkGinPermission(s.dbModel, ctx)
}

// UpdateAttachment 更新附件。只允许更新附件名称、是否合法以及描述字段
func (s *AttachmentAPIService) UpdateAttachment(ctx context.Context, req *pb.Attachment) (*emptypb.Empty, error) {
	_, err := s.checkPermission(ctx)
	if err != nil {
		return nil, err
	}

	updateFields := []string{"name", "enable", "description"}
	err = s.dbModel.UpdateAttachment(&model.Attachment{
		Id:          req.Id,
		Name:        req.Name,
		Description: req.Description,
		Enable:      req.Enable,
	}, updateFields...)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &emptypb.Empty{}, nil
}

func (s *AttachmentAPIService) DeleteAttachment(ctx context.Context, req *pb.DeleteAttachmentRequest) (*emptypb.Empty, error) {
	_, err := s.checkPermission(ctx)
	if err != nil {
		return nil, err
	}

	err = s.dbModel.DeleteAttachment(req.Id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &emptypb.Empty{}, nil
}

// GetAttachment 查询单个附件信息
func (s *AttachmentAPIService) GetAttachment(ctx context.Context, req *pb.GetAttachmentRequest) (*pb.Attachment, error) {
	_, err := s.checkPermission(ctx)
	if err != nil {
		return nil, err
	}

	attachment, err := s.dbModel.GetAttachment(req.Id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	pbAttachment := &pb.Attachment{}
	util.CopyStruct(&attachment, pbAttachment)

	return pbAttachment, nil
}

func (s *AttachmentAPIService) ListAttachment(ctx context.Context, req *pb.ListAttachmentRequest) (*pb.ListAttachmentReply, error) {
	_, err := s.checkPermission(ctx)
	if err != nil {
		return nil, err
	}

	opt := &model.OptionGetAttachmentList{
		Page:      int(req.Page),
		Size:      int(req.Size_),
		WithCount: true,
		QueryIn:   make(map[string][]interface{}),
	}

	if len(req.UserId) > 0 {
		opt.QueryIn["user_id"] = util.Slice2Interface(req.UserId)
	}

	if len(req.Enable) > 0 {
		opt.QueryIn["enable"] = util.Slice2Interface(req.Enable)
	}

	if len(req.Type) > 0 {
		opt.QueryIn["type"] = util.Slice2Interface(req.Type)
	}

	req.Wd = strings.TrimSpace(req.Wd)
	if req.Wd != "" {
		wd := "%" + req.Wd + "%"
		opt.QueryLike = map[string][]interface{}{"name": {wd}, "description": {wd}}
	}

	attachments, total, err := s.dbModel.GetAttachmentList(opt)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	var pbAttachments []*pb.Attachment
	util.CopyStruct(&attachments, &pbAttachments)

	var (
		userIds        []interface{}
		userIdIndexMap = make(map[int64][]int)
	)

	for idx, attchment := range pbAttachments {
		attchment.TypeName = s.dbModel.GetAttachmentTypeName(int(attchment.Type))
		userIds = append(userIds, attchment.UserId)
		userIdIndexMap[attchment.UserId] = append(userIdIndexMap[attchment.UserId], idx)
		pbAttachments[idx] = attchment
	}

	if size := len(userIds); size > 0 {
		users, _, _ := s.dbModel.GetUserList(&model.OptionGetUserList{Ids: userIds, Page: 1, Size: size, SelectFields: []string{"id", "username"}})
		s.logger.Debug("GetUserList", zap.Any("users", users))
		for _, user := range users {
			if indexes, ok := userIdIndexMap[user.Id]; ok {
				for _, idx := range indexes {
					pbAttachments[idx].Username = user.Username
				}
			}
		}
	}
	return &pb.ListAttachmentReply{Total: total, Attachment: pbAttachments}, nil
}

// UploadDocument 上传文档
func (s *AttachmentAPIService) UploadDocument(ctx *gin.Context) {
	userCliams, statusCodes, err := s.checkGinPermission(ctx)
	if err != nil {
		ctx.JSON(statusCodes, ginResponse{Code: statusCodes, Message: err.Error(), Error: err.Error()})
		return
	}

	form, err := ctx.MultipartForm()
	if err != nil {
		ctx.JSON(http.StatusBadRequest, ginResponse{Code: http.StatusBadRequest, Message: err.Error(), Error: err.Error()})
		return
	}

	var attachments []*model.Attachment

	name := "file"
	fileheaders := form.File[name]
	for _, fileheader := range fileheaders {
		ext := strings.ToLower(filepath.Ext(fileheader.Filename))
		if !filetil.IsDocument(ext) {
			ctx.JSON(http.StatusBadRequest, ginResponse{Code: http.StatusBadRequest, Message: "不支持的文件类型", Error: "不支持的文件类型"})
			return
		}
		attachment, err := s.saveFile(ctx, fileheader)
		if err != nil {
			os.Remove("." + attachment.Path)
			ctx.JSON(http.StatusInternalServerError, ginResponse{Code: http.StatusInternalServerError, Message: err.Error(), Error: err.Error()})
			return
		}
		attachment.UserId = userCliams.UserId
		attachment.Type = model.AttachmentTypeDocument
		attachments = append(attachments, attachment)
	}

	err = s.dbModel.CreateAttachments(attachments)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, ginResponse{Code: http.StatusInternalServerError, Message: err.Error(), Error: err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, ginResponse{Code: http.StatusOK, Message: "ok", Data: attachments})
}

// UploadAvatar 上传头像
func (s *AttachmentAPIService) UploadAvatar(ctx *gin.Context) {
	s.uploadImage(ctx, model.AttachmentTypeAvatar)
}

//  UploadArticle 上传文章相关图片和视频。这里不验证文件格式。
// 注意：当前适配了wangeditor的接口规范，如果需要适配其他编辑器，需要修改此接口或者增加其他接口
func (s *AttachmentAPIService) UploadArticle(ctx *gin.Context) {
	typ := ctx.Query("type")
	if typ != "image" && typ != "video" {
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 1, "msg": "类型参数错误"})
		return
	}

	userCliams, _, err := s.checkGinPermission(ctx)
	if err != nil {
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 1, "msg": err.Error()})
		return
	}

	name := "file"
	fileHeader, err := ctx.FormFile(name)
	if err != nil {
		s.logger.Error("MultipartForm", zap.Error(err))
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 1, "msg": err.Error()})
		return
	}

	attachment, err := s.saveFile(ctx, fileHeader)
	if err != nil {
		s.logger.Error("saveFile", zap.Error(err))
		os.Remove("." + attachment.Path)
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 1, "msg": err.Error()})
		return
	}
	attachment.UserId = userCliams.UserId
	attachment.Type = model.AttachmentTypeArticle

	err = s.dbModel.CreateAttachment(attachment)
	if err != nil {
		s.logger.Error("CreateAttachments", zap.Error(err))
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 1, "msg": err.Error()})
		return
	}

	if typ == "image" {
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 0, "data": map[string]interface{}{
			"url": attachment.Path,
			"alt": attachment.Name,
			// "href": "",
		}})
	} else {
		ctx.JSON(http.StatusOK, map[string]interface{}{"errno": 0, "data": map[string]interface{}{
			"url": attachment.Path,
			// "poster": "",
		}})
	}
}

// UploadBanner 上传横幅，创建横幅的时候，要根据附件id，更新附件的type_id字段
func (s *AttachmentAPIService) UploadBanner(ctx *gin.Context) {
	s.uploadImage(ctx, model.AttachmentTypeBanner)
}

// 上传文档分类封面
func (s *AttachmentAPIService) UploadCategoryCover(ctx *gin.Context) {
	s.uploadImage(ctx, model.AttachmentTypeCategoryCover)
}

func (s *AttachmentAPIService) uploadImage(ctx *gin.Context, attachmentType int) {
	name := "file"
	userCliams, statusCodes, err := s.checkGinPermission(ctx)
	if err != nil {
		ctx.JSON(statusCodes, ginResponse{Code: statusCodes, Message: err.Error(), Error: err.Error()})
		return
	}

	// 验证文件是否是图片
	fileHeader, err := ctx.FormFile(name)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, ginResponse{Code: http.StatusBadRequest, Message: err.Error(), Error: err.Error()})
		return
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !filetil.IsImage(ext) {
		message := "请上传图片格式文件，支持.jpg、.jpeg和.png格式图片"
		ctx.JSON(http.StatusBadRequest, ginResponse{Code: http.StatusBadRequest, Message: message, Error: message})
		return
	}

	attachment, err := s.saveFile(ctx, fileHeader)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, ginResponse{Code: http.StatusBadRequest, Message: err.Error(), Error: err.Error()})
		return
	}
	attachment.Type = attachmentType
	attachment.UserId = userCliams.UserId

	if attachmentType == model.AttachmentTypeAvatar {
		attachment.TypeId = userCliams.UserId
		// 更新用户头像信息
		err = s.dbModel.UpdateUser(&model.User{Id: userCliams.UserId, Avatar: attachment.Path}, "avatar")
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, ginResponse{Code: http.StatusInternalServerError, Message: err.Error(), Error: err.Error()})
		}
	}

	// 保存附件信息
	err = s.dbModel.CreateAttachment(attachment)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, ginResponse{Code: http.StatusInternalServerError, Message: err.Error(), Error: err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, ginResponse{Code: http.StatusOK, Message: "上传成功", Data: attachment})
}

// saveFile 保存文件。文件以md5值命名以及存储
// 同时，返回附件信息
func (s *AttachmentAPIService) saveFile(ctx *gin.Context, fileHeader *multipart.FileHeader) (attachment *model.Attachment, err error) {
	cacheDir := fmt.Sprintf("cache/uploads/%s", time.Now().Format("2006/01/02"))
	os.MkdirAll(cacheDir, os.ModePerm)
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	cachePath := fmt.Sprintf("%s/%s%s", cacheDir, uuid.Must(uuid.NewV4()).String(), ext)
	defer func() {
		os.Remove(cachePath)
	}()

	// 保存到临时文件
	err = ctx.SaveUploadedFile(fileHeader, cachePath)
	if err != nil {
		s.logger.Error("SaveUploadedFile", zap.Error(err), zap.String("filename", fileHeader.Filename), zap.String("cachePath", cachePath))
		return
	}

	// 获取文件md5值
	md5hash, errHash := filetil.GetFileMD5(cachePath)
	if errHash != nil {
		err = errHash
		return
	}

	savePath := fmt.Sprintf("uploads/%s/%s%s", strings.Join(strings.Split(md5hash, "")[0:5], "/"), md5hash, ext)
	os.MkdirAll(filepath.Dir(savePath), os.ModePerm)
	err = util.CopyFile(cachePath, savePath)
	if err != nil {
		s.logger.Error("Rename", zap.Error(err), zap.String("cachePath", cachePath), zap.String("savePath", savePath))
		return
	}

	attachment = &model.Attachment{
		Size:   fileHeader.Size,
		Name:   fileHeader.Filename,
		Ip:     ctx.ClientIP(),
		Ext:    ext,
		Enable: true, // 默认都是合法的
		Hash:   md5hash,
		Path:   "/" + savePath,
	}

	// 对于图片，直接获取图片的宽高
	if filetil.IsImage(ext) {
		attachment.Width, attachment.Height, _ = filetil.GetImageSize(cachePath)
	}

	return
}
