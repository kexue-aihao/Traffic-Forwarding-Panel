# 支付协议来源与验证边界

本文件记录适配器实际实现的协议版本、可复查依据和测试层级。协议测试通过不代表已有可用商户，未配置商户不得展示为支付上线验收通过。Cyber 未取得确定协议，仍为外部依赖，不生成猜测实现。

## 固定版本与来源

| 适配器 | 固定来源 | 使用文件及范围 |
|---|---|---|
| 原版 EPUSDT | `GMWalletApp/epusdt` 历史 `v0.0.1`，提交 `7b1512fae1a918984fb3aa0f55d1a2362d49ed2a` | `wiki/API.md`、`src/util/sign/sign.go`、`src/route/router.go`：创建交易、回调、MD5签名及已公布向量 |
| BEpusdt | `v03413/bepusdt` `v1.24.2`，提交 `4d88040fd4096e77e8fb9ad2650e775753a977b6` | `app/router/epusdt.go`、`app/handler/epusdt/epusdt.go`、`app/utils/utils.go`、`app/model/order.go`、`app/task/notify/notify.go`、`docs/api/api.md` |
| TokenPay | `LightCountry/TokenPay`，提交 `407471ea25f6b6f9d704690735d30b34d2b1f0ac` | `Wiki/docs.md`、`src/TokenPay/Controllers/HomeController.cs`、`src/TokenPay/Extensions/ObjectExtension.cs`：创建、查单、完整回调字典和MD5/HMAC向量 |
| Cryptomus | 官方 `CryptomusCom/api-php-sdk`，提交 `6993cd27871598b814479960d7059db653772084` | `src/RequestBuilder.php`、`src/Payment.php`；另核对下列官方在线文档 |
| EPay MD5 | 彩虹易支付实现镜像 `maajiko/Epay`，提交 `a4d0f0421cfcbc573913c8007bde3d0c04a5713a` | `plugins/epay/inc/EpayCore.class.php`、`api.php`：submit、mapi、按商户凭据查询。不是所有“易支付”分支的兼容承诺 |

可复查永久链接：

- [EPUSDT 历史 API](https://github.com/GMWalletApp/epusdt/blob/7b1512fae1a918984fb3aa0f55d1a2362d49ed2a/wiki/API.md)
- [BEpusdt 路由](https://github.com/v03413/bepusdt/blob/4d88040fd4096e77e8fb9ad2650e775753a977b6/app/router/epusdt.go)
- [BEpusdt 签名源码](https://github.com/v03413/bepusdt/blob/4d88040fd4096e77e8fb9ad2650e775753a977b6/app/utils/utils.go)
- [TokenPay API 与向量](https://github.com/LightCountry/TokenPay/blob/407471ea25f6b6f9d704690735d30b34d2b1f0ac/Wiki/docs.md)
- [TokenPay 完整回调字段](https://github.com/LightCountry/TokenPay/blob/407471ea25f6b6f9d704690735d30b34d2b1f0ac/src/TokenPay/Extensions/ObjectExtension.cs)
- [Cryptomus SDK](https://github.com/CryptomusCom/api-php-sdk/blob/6993cd27871598b814479960d7059db653772084/src/RequestBuilder.php)
- [EPay SDK 镜像](https://github.com/maajiko/Epay/blob/a4d0f0421cfcbc573913c8007bde3d0c04a5713a/plugins/epay/inc/EpayCore.class.php)
- Cryptomus 官方文档（2026-09-20 查阅，无固定版本号）：[请求格式](https://doc.cryptomus.com/merchant-api/request-format)、[创建支付](https://doc.cryptomus.com/merchant-api/payments/creating-invoice)、[查单](https://doc.cryptomus.com/merchant-api/payments/payment-information)、[回调](https://doc.cryptomus.com/merchant-api/payments/webhook)。

`assimon/epusdt` 已重定向到上述仓库，现行主分支发生产品变更，不能直接当作原版协议。原版源码包含空字符串参与签名，而文档描述忽略空值；本实现按固定源码处理原版，创建请求不发送空字段。BEpusdt 按其源码忽略空值，并保持数值规范化差异。

## 实现范围

| Kind | 创建 | 查单 | 回调入账条件 | 成功回调正文 |
|---|---|---|---|---|
| `epay` | `submit.php`跳转，或显式`mapi.php` | `api.php?act=order` | 商户、MD5、`TRADE_SUCCESS`；按订单精确核对money | `success` |
| `epusdt` | `/api/v1/order/create-transaction` | 明确返回`ErrUnsupported` | 完整字段MD5、状态2；amount为CNY | `ok` |
| `bepusdt` | `/api/v1/order/create-order`独立模式 | `/api/v1/pay/info` | 完整字段MD5、状态2；amount为创建时CNY | `success` |
| `tokenpay` | `/CreateOrder` | `/Query?Id&Signature` | 显式选定MD5或HMAC-SHA256、Status=1、BaseCurrency=CNY、Currency符合配置 | `ok` |
| `cryptomus` | `/v1/payment` | `/v1/payment/info` | 签名、type=payment、currency=CNY、状态paid或paid_over | HTTP 200；本实现正文`ok` |

BEpusdt 查单端点本身无签名认证，并可能绑定收银台浏览器指纹。本适配器只通过已配置服务器的验证HTTPS获取结果；端点拒绝时明确失败，不伪造“已支付”。其源码扩展状态4=取消、5=确认中、6=失败均不会记为付款成功。回调源码没有fiat字段，因此需使用固定为CNY创建的本地订单快照核对；存在fiat字段且不是CNY时拒绝。

TokenPay `ActualAmount`为法币金额，`Amount`为加密货币金额。不能将后者当作人民币余额。签名包含所有收到的标量字段，包括新增的`SignatureType`及布尔字段；不按历史固定字段列表过滤。HMAC模式不会同时接受MD5。查单字段大小写按源码`SortedDictionary`保留。

Cryptomus 请求签名为`MD5(base64(实际发送JSON字节)+key)`。回调按照官方PHP示例移除根级sign、保留对象键原顺序、保留Unicode并转义斜杠，随后计算签名。不能先转成Go map再排序回编码。重复JSON键拒绝。普通金额字符串精确解析为人民币分，不使用float计价。回调若引入极大数值或指数表示的数值型附加字段，PHP浮点编码差异可能导致拒绝，需要用真实回调扩展兼容测试；不会跳过验签。

EPay mapi需要ClientIP，返回HTTPS支付链接或HTTPS二维码链接；不接受任意应用深链作为页面跳转。EPay传统查单协议要求URL中包含商户key：实现不记录URL，网络错误不会返回完整URL或key，部署端必须避免代理记录带凭据查询串。未照搬参考PHP SDK关闭证书验证的做法。

## 安全与商业接线要求

所有发送均受15秒上限、调用方context及1MiB响应限制；禁止重定向，默认校验TLS证书，自托管网关仅允许预先配置的HTTPS地址和固定协议路径。Cryptomus对外配置固定官方API域名。回调限制1MiB、拒绝重复字段和不匹配Content-Type，不记录密钥。

`VerifyNotify`只验证协议并返回规范化Status，不直接操作账本。商业层必须：

1. 使用订单保存的渠道实例及密钥版本验签，不能由回调任选通道。
2. 调用`ValidateExpected`核对商户订单号、人民币分、CNY及非空渠道交易号，并与已绑定的交易号核对。
3. 只有`State=Paid`才进入持久化唯一约束下的付款事务；交易号去重范围至少包含通道实例。
4. 合法重复通知仍应返回该通道成功正文，但不得重复入账；签名合法不代表未处理过。
5. 创建/查询失败、未知状态、金额不一致、晚到支付和退款进入明确业务流程；网页return不作为到账依据。

## 已运行验证及尚需验证

已运行`go test ./internal/payment -count=1`和`go vet ./internal/payment`：EPUSDT已公布签名向量、TokenPay已公布MD5/HMAC创建及查询向量、完整官方回调向量；五类接口的`httptest.NewTLSServer`协议模拟，使用测试CA的可信Client，没有禁用TLS校验；未受信证书、重定向、超时、超大响应、金额/币种篡改、重复字段、算法降级及重放状态稳定性。

Cryptomus测试使用官方规范构造的跨运行时固定向量，由Node.js `crypto`独立生成expected值，**不是供应商公开的密钥/签名测试向量**。该向量包含中文、斜杠和嵌套对象，用于防止签名自我验证掩盖键排序错误。

尚未运行：真实商户沙箱/小额支付、供应商真实回调重试、真实EPay分支差异、真实BEpusdt浏览器指纹查单行为，以及商业层接线后的每渠道全闭环。不得将本文件协议测试记为上述线上验收通过。
