package remote

// TODO 测试
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	model "github.com/HFO4/cloudreve/models"
	"github.com/HFO4/cloudreve/pkg/auth"
	"github.com/HFO4/cloudreve/pkg/filesystem/fsctx"
	"github.com/HFO4/cloudreve/pkg/filesystem/response"
	"github.com/HFO4/cloudreve/pkg/request"
	"github.com/HFO4/cloudreve/pkg/serializer"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Handler 远程存储策略适配器
type Handler struct {
	Client       request.Client
	Policy       *model.Policy
	AuthInstance auth.Auth
}

// getAPIUrl 获取接口请求地址
func (handler Handler) getAPIUrl(scope string) string {
	serverURL, err := url.Parse(handler.Policy.Server)
	if err != nil {
		return ""
	}
	var controller *url.URL

	switch scope {
	case "delete":
		controller, _ = url.Parse("/api/v3/slave/delete")
	case "thumb":
		controller, _ = url.Parse("/api/v3/slave/thumb")
	}

	return serverURL.ResolveReference(controller).String()
}

// Get 获取文件内容
func (handler Handler) Get(ctx context.Context, path string) (response.RSCloser, error) {
	// 尝试获取速度限制 TODO 是否需要在这里限制？
	speedLimit := 0
	if user, ok := ctx.Value(fsctx.UserCtx).(model.User); ok {
		speedLimit = user.Group.SpeedLimit
	}

	// 获取文件源地址
	downloadURL, err := handler.Source(ctx, path, url.URL{}, 0, true, speedLimit)
	if err != nil {
		return nil, err
	}

	// 获取文件数据流
	resp, err := handler.Client.Request(
		"GET",
		downloadURL,
		nil,
		request.WithContext(ctx),
	).CheckHTTPResponse(200).GetRSCloser()

	if err != nil {
		return nil, err
	}

	return resp, nil
}

// Put 将文件流保存到指定目录
func (handler Handler) Put(ctx context.Context, file io.ReadCloser, dst string, size uint64) error {
	return errors.New("远程策略不支持此上传方式")
}

// Delete 删除一个或多个文件，
// 返回未删除的文件，及遇到的最后一个错误
func (handler Handler) Delete(ctx context.Context, files []string) ([]string, error) {
	// 封装接口请求正文
	reqBody := serializer.RemoteDeleteRequest{
		Files: files,
	}
	reqBodyEncoded, err := json.Marshal(reqBody)
	if err != nil {
		return files, err
	}

	// 发送删除请求
	bodyReader := strings.NewReader(string(reqBodyEncoded))
	signTTL := model.GetIntSetting("slave_api_timeout", 60)
	resp, err := handler.Client.Request(
		"POST",
		handler.getAPIUrl("delete"),
		bodyReader,
		request.WithCredential(handler.AuthInstance, int64(signTTL)),
	).CheckHTTPResponse(200).GetResponse()
	if err != nil {
		return files, err
	}

	// 处理删除结果
	var reqResp serializer.Response
	err = json.Unmarshal([]byte(resp), &reqResp)
	if err != nil {
		return files, err
	}
	if reqResp.Code != 0 {
		var failedResp serializer.RemoteDeleteRequest
		if failed, ok := reqResp.Data.(string); ok {
			err = json.Unmarshal([]byte(failed), &failedResp)
			if err == nil {
				return failedResp.Files, errors.New(reqResp.Error)
			}
		}
		return files, errors.New("未知的返回结果格式")
	}

	return []string{}, nil
}

// Thumb 获取文件缩略图
func (handler Handler) Thumb(ctx context.Context, path string) (*response.ContentResponse, error) {
	sourcePath := base64.RawURLEncoding.EncodeToString([]byte(path))
	thumbURL := handler.getAPIUrl("thumb") + "/" + sourcePath
	ttl := model.GetIntSetting("slave_api_timeout", 60)
	signedThumbURL, err := auth.SignURI(handler.AuthInstance, thumbURL, int64(ttl))
	if err != nil {
		return nil, err
	}

	return &response.ContentResponse{
		Redirect: true,
		URL:      signedThumbURL.String(),
	}, nil
}

// Source 获取外链URL
func (handler Handler) Source(
	ctx context.Context,
	path string,
	baseURL url.URL,
	ttl int64,
	isDownload bool,
	speed int,
) (string, error) {
	// 尝试从上下文获取文件名
	fileName := "file"
	if file, ok := ctx.Value(fsctx.FileModelCtx).(model.File); ok {
		fileName = file.Name
	}

	serverURL, err := url.Parse(handler.Policy.Server)
	if err != nil {
		return "", errors.New("无法解析远程服务端地址")
	}

	var (
		signedURI  *url.URL
		controller = "/api/v3/slave/download"
	)
	if !isDownload {
		controller = "/api/v3/slave/source"
	}

	// 签名下载地址
	sourcePath := base64.RawURLEncoding.EncodeToString([]byte(path))
	signedURI, err = auth.SignURI(
		handler.AuthInstance,
		fmt.Sprintf("%s/%d/%s/%s", controller, speed, sourcePath, fileName),
		ttl,
	)

	if err != nil {
		return "", serializer.NewError(serializer.CodeEncryptError, "无法对URL进行签名", err)
	}

	finalURL := serverURL.ResolveReference(signedURI).String()
	return finalURL, nil

}

// Token 获取上传策略和认证Token
func (handler Handler) Token(ctx context.Context, TTL int64, key string) (serializer.UploadCredential, error) {
	// 生成回调地址
	siteURL := model.GetSiteURL()
	apiBaseURI, _ := url.Parse("/api/v3/callback/remote/" + key)
	apiURL := siteURL.ResolveReference(apiBaseURI)

	// 生成上传策略
	policy := serializer.UploadPolicy{
		SavePath:         handler.Policy.DirNameRule,
		FileName:         handler.Policy.FileNameRule,
		AutoRename:       handler.Policy.AutoRename,
		MaxSize:          handler.Policy.MaxSize,
		AllowedExtension: handler.Policy.OptionsSerialized.FileType,
		CallbackURL:      apiURL.String(),
	}
	policyEncoded, err := policy.EncodeUploadPolicy()
	if err != nil {
		return serializer.UploadCredential{}, err
	}

	// 签名上传策略
	uploadRequest, _ := http.NewRequest("POST", "/api/v3/slave/upload", nil)
	uploadRequest.Header = map[string][]string{
		"X-Policy": {policyEncoded},
	}
	auth.SignRequest(handler.AuthInstance, uploadRequest, TTL)

	if credential, ok := uploadRequest.Header["Authorization"]; ok && len(credential) == 1 {
		return serializer.UploadCredential{
			Token:  credential[0],
			Policy: policyEncoded,
		}, nil
	}
	return serializer.UploadCredential{}, errors.New("无法签名上传策略")

}