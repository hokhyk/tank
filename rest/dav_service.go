package rest

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"tank/rest/dav"
)

/**
 *
 * WebDav协议文档
 * https://tools.ietf.org/html/rfc4918
 * 主要参考 golang.org/x/net/webdav
 */
//@Service
type DavService struct {
	Bean
	matterDao     *MatterDao
	matterService *MatterService
}

//初始化方法
func (this *DavService) Init() {
	this.Bean.Init()

	//手动装填本实例的Bean. 这里必须要用中间变量方可。
	b := CONTEXT.GetBean(this.matterDao)
	if b, ok := b.(*MatterDao); ok {
		this.matterDao = b
	}

	b = CONTEXT.GetBean(this.matterService)
	if b, ok := b.(*MatterService); ok {
		this.matterService = b
	}

}

//获取Header头中的Depth值，暂不支持 infinity
func (this *DavService) ParseDepth(request *http.Request) int {

	depth := 1
	if hdr := request.Header.Get("Depth"); hdr != "" {
		switch hdr {
		case "0":
			return 0
		case "1":
			return 1
		case "infinity":
			return 1
		}
	} else {
		this.PanicBadRequest("必须指定Header Depth")
	}
	return depth
}

func (this *DavService) makePropstatResponse(href string, pstats []dav.Propstat) *dav.Response {
	resp := dav.Response{
		Href:     []string{(&url.URL{Path: href}).EscapedPath()},
		Propstat: make([]dav.SubPropstat, 0, len(pstats)),
	}
	for _, p := range pstats {
		var xmlErr *dav.XmlError
		if p.XMLError != "" {
			xmlErr = &dav.XmlError{InnerXML: []byte(p.XMLError)}
		}
		resp.Propstat = append(resp.Propstat, dav.SubPropstat{
			Status:              fmt.Sprintf("HTTP/1.1 %d %s", p.Status, dav.StatusText(p.Status)),
			Prop:                p.Props,
			ResponseDescription: p.ResponseDescription,
			Error:               xmlErr,
		})
	}
	return &resp
}

//从一个matter中获取其 []dav.Propstat
func (this *DavService) PropstatsFromXmlNames(user *User, matter *Matter, xmlNames []xml.Name) []dav.Propstat {

	propstats := make([]dav.Propstat, 0)

	var properties []dav.Property

	for _, xmlName := range xmlNames {
		//TODO: deadprops尚未考虑

		// Otherwise, it must either be a live property or we don't know it.
		if liveProp := LivePropMap[xmlName]; liveProp.findFn != nil && (liveProp.dir || !matter.Dir) {
			innerXML := liveProp.findFn(user, matter)

			properties = append(properties, dav.Property{
				XMLName:  xmlName,
				InnerXML: []byte(innerXML),
			})
		} else {
			this.logger.Info("%s的%s无法完成", matter.Path, xmlName.Local)
		}
	}

	if len(properties) == 0 {
		this.PanicBadRequest("请求的属性项无法解析！")
	}

	okPropstat := dav.Propstat{Status: http.StatusOK, Props: properties}

	propstats = append(propstats, okPropstat)

	return propstats

}

//从一个matter中获取所有的propsNames
func (this *DavService) AllPropXmlNames(matter *Matter) []xml.Name {

	pnames := make([]xml.Name, 0)
	for pn, prop := range LivePropMap {
		if prop.findFn != nil && (prop.dir || !matter.Dir) {
			pnames = append(pnames, pn)
		}
	}

	return pnames
}

//从一个matter中获取其 []dav.Propstat
func (this *DavService) Propstats(user *User, matter *Matter, propfind dav.Propfind) []dav.Propstat {

	propstats := make([]dav.Propstat, 0)
	if propfind.Propname != nil {
		this.PanicBadRequest("propfind.Propname != nil 尚未处理")
	} else if propfind.Allprop != nil {

		//TODO: 如果include中还有内容，那么包含进去。
		xmlNames := this.AllPropXmlNames(matter)

		propstats = this.PropstatsFromXmlNames(user, matter, xmlNames)

	} else {
		propstats = this.PropstatsFromXmlNames(user, matter, propfind.Prop)
	}

	return propstats

}

//列出文件夹或者目录详情
func (this *DavService) HandlePropfind(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("PROPFIND %s\n", subPath)

	//获取请求的层数。暂不支持 infinity
	depth := this.ParseDepth(request)

	//读取请求参数。按照用户的参数请求返回内容。
	propfind, _, err := dav.ReadPropfind(request.Body)
	this.PanicError(err)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if subPath == "" || subPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, subPath)
	}

	var matters []*Matter
	if depth == 0 {
		matters = []*Matter{matter}
	} else {
		// len(matters) == 0 表示该文件夹下面是空文件夹
		matters = this.matterDao.List(matter.Uuid, user.Uuid, nil)

		//将当前的matter添加到头部
		matters = append([]*Matter{matter}, matters...)
	}

	//准备一个输出结果的Writer
	multiStatusWriter := dav.MultiStatusWriter{Writer: writer}

	for _, matter := range matters {

		fmt.Printf("处理Matter %s\n", matter.Path)

		propstats := this.Propstats(user, matter, propfind)
		path := fmt.Sprintf("%s%s", WEBDAV_PREFFIX, matter.Path)
		response := this.makePropstatResponse(path, propstats)

		err = multiStatusWriter.Write(response)
		this.PanicError(err)
	}

	//闭合
	err = multiStatusWriter.Close()
	this.PanicError(err)

	fmt.Printf("%v %v \n", subPath, propfind.Prop)

}

//请求文件详情（下载）
func (this *DavService) HandleGetHeadPost(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("GET %s\n", subPath)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if subPath == "" || subPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, subPath)
	}

	//如果是文件夹，相当于是 Propfind
	if matter.Dir {
		this.HandlePropfind(writer, request, user, subPath)
		return
	}

	//下载一个文件。
	this.matterService.DownloadFile(writer, request, matter.AbsolutePath(), matter.Name, false)

}

//上传文件
func (this *DavService) HandlePut(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("PUT %s\n", subPath)

	filename := GetFilenameOfPath(subPath)
	dirPath := GetDirOfPath(subPath)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if dirPath == "" || dirPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, dirPath)
	}


	//如果存在，那么先删除再说。
	srcMatter := this.matterDao.findByUserUuidAndPath(user.Uuid, subPath)
	if srcMatter != nil {
		this.matterDao.Delete(srcMatter)
	}

	this.matterService.Upload(request.Body, user, matter.Uuid, filename, true, false)

}

//删除文件
func (this *DavService) HandleDelete(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("DELETE %s\n", subPath)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if subPath == "" || subPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, subPath)
	}

	this.matterDao.Delete(matter)
}

//创建文件夹
func (this *DavService) HandleMkcol(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("MKCOL %s\n", subPath)

	thisDirName := GetFilenameOfPath(subPath)
	dirPath := GetDirOfPath(subPath)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if dirPath == "" || dirPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, dirPath)
	}

	this.matterService.CreateDirectory(matter.Uuid, thisDirName, user)

}

//跨域请求的OPTIONS询问
func (this *DavService) HandleOptions(w http.ResponseWriter, r *http.Request, user *User, subPath string) {

	fmt.Printf("OPTIONS %s\n", subPath)

	//寻找符合条件的matter.
	var matter *Matter
	//如果是空或者/就是请求根目录
	if subPath == "" || subPath == "/" {
		matter = NewRootMatter(user)
	} else {
		matter = this.matterDao.checkByUserUuidAndPath(user.Uuid, subPath)
	}

	allow := "OPTIONS, LOCK, PUT, MKCOL"
	if matter.Dir {
		allow = "OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND"
	} else {
		allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND, PUT"
	}

	w.Header().Set("Allow", allow)
	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")

}

//移动或者复制的准备工作
func (this *DavService) prepareMoveCopy(
	writer http.ResponseWriter,
	request *http.Request,
	user *User, subPath string) (
	srcMatter *Matter,
	destDirMatter *Matter,
	srcDirPath string,
	destinationDirPath string,
	destinationName string) {

	//解析出目标路径。
	destinationStr := request.Header.Get("Destination")

	//解析出Overwrite。
	overwriteStr := request.Header.Get("Overwrite")

	//有前缀的目标path
	var fullDestinationPath string
	//去掉前缀的目标path
	var destinationPath string

	if destinationStr == "" {
		this.PanicBadRequest("Header Destination必填")
	}

	//如果是重命名，那么就不是http开头了。
	if strings.HasPrefix(destinationStr, WEBDAV_PREFFIX) {
		fullDestinationPath = destinationStr
	} else {
		destinationUrl, err := url.Parse(destinationStr)
		this.PanicError(err)
		if destinationUrl.Host != request.Host {
			this.PanicBadRequest("Destination Host不一致. %s  %s != %s", destinationStr, destinationUrl.Host, request.Host)
		}
		fullDestinationPath = destinationUrl.Path
	}

	//去除前缀
	pattern := fmt.Sprintf(`^%s(.*)$`, WEBDAV_PREFFIX)
	reg := regexp.MustCompile(pattern)
	strs := reg.FindStringSubmatch(fullDestinationPath)
	if len(strs) == 2 {
		destinationPath = strs[1]
	} else {
		this.PanicBadRequest("目标前缀必须为：%s", WEBDAV_PREFFIX)
	}

	destinationName = GetFilenameOfPath(destinationPath)
	destinationDirPath = GetDirOfPath(destinationPath)
	srcDirPath = GetDirOfPath(subPath)

	overwrite := false
	if overwriteStr == "T" {
		overwrite = true
	}

	//如果前后一致，那么相当于没有改变
	if destinationPath == subPath {
		return
	}

	//源matter.
	//如果是空或者/就是请求根目录
	if subPath == "" || subPath == "/" {
		this.PanicBadRequest("你不能移动根目录！")
	} else {
		srcMatter = this.matterDao.checkByUserUuidAndPath(user.Uuid, subPath)
	}

	//目标matter
	destMatter := this.matterDao.findByUserUuidAndPath(user.Uuid, destinationPath)

	//目标文件夹matter
	if destinationDirPath == "" || destinationDirPath == "/" {
		destDirMatter = NewRootMatter(user)
	} else {
		destDirMatter = this.matterDao.checkByUserUuidAndPath(user.Uuid, destinationDirPath)
	}

	//如果目标matter存在了。
	if destMatter != nil {

		//如果目标matter还存在了。
		if overwrite {
			//要求覆盖。那么删除。
			this.matterDao.Delete(destMatter)
		} else {
			this.PanicBadRequest("%s已经存在，操作失败！", destinationName)
		}
	}

	return srcMatter, destDirMatter, srcDirPath, destinationDirPath, destinationName

}

//移动或者重命名
func (this *DavService) HandleMove(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("MOVE %s\n", subPath)

	srcMatter, destDirMatter, srcDirPath, destinationDirPath, destinationName := this.prepareMoveCopy(writer, request, user, subPath)
	//移动到新目录中去。
	if destinationDirPath == srcDirPath {
		//文件夹没变化，相当于重命名。
		this.matterService.Rename(srcMatter, destinationName, user)
	} else {
		this.matterService.Move(srcMatter, destDirMatter)
	}

	this.logger.Info("完成移动 %s => %s", subPath, destinationDirPath)
}

//复制文件/文件夹
func (this *DavService) HandleCopy(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	fmt.Printf("COPY %s\n", subPath)

	srcMatter, destDirMatter, _, destinationDirPath, destinationName := this.prepareMoveCopy(writer, request, user, subPath)

	//复制到新目录中去。
	this.matterService.Copy(srcMatter, destDirMatter, destinationName)

	this.logger.Info("完成复制 %s => %s", subPath, destinationDirPath)

}

//加锁
func (this *DavService) HandleLock(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	this.PanicBadRequest("不支持LOCK方法")
}

//解锁
func (this *DavService) HandleUnlock(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	this.PanicBadRequest("不支持UNLOCK方法")
}

//修改文件属性
func (this *DavService) HandleProppatch(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	this.PanicBadRequest("不支持PROPPATCH方法")
}

//处理所有的请求
func (this *DavService) HandleDav(writer http.ResponseWriter, request *http.Request, user *User, subPath string) {

	method := request.Method
	if method == "OPTIONS" {

		//跨域问询
		this.HandleOptions(writer, request, user, subPath)

	} else if method == "GET" || method == "HEAD" || method == "POST" {

		//请求文件详情（下载）
		this.HandleGetHeadPost(writer, request, user, subPath)

	} else if method == "DELETE" {

		//删除文件
		this.HandleDelete(writer, request, user, subPath)

	} else if method == "PUT" {

		//上传文件
		this.HandlePut(writer, request, user, subPath)

	} else if method == "MKCOL" {

		//创建文件夹
		this.HandleMkcol(writer, request, user, subPath)

	} else if method == "COPY" {

		//复制文件/文件夹
		this.HandleCopy(writer, request, user, subPath)

	} else if method == "MOVE" {

		//移动（重命名）文件/文件夹
		this.HandleMove(writer, request, user, subPath)

	} else if method == "LOCK" {

		//加锁
		this.HandleLock(writer, request, user, subPath)

	} else if method == "UNLOCK" {

		//释放锁
		this.HandleUnlock(writer, request, user, subPath)

	} else if method == "PROPFIND" {

		//列出文件夹或者目录详情
		this.HandlePropfind(writer, request, user, subPath)

	} else if method == "PROPPATCH" {

		//修改文件属性
		this.HandleProppatch(writer, request, user, subPath)

	} else {

		this.PanicBadRequest("该方法还不支持。%s", method)

	}

}