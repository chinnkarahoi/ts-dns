# Telescope DNS
带GFWList&反污染功能的DNS分流器
![](arch.png)

## 编译&使用

```shell
go build -o ts-dns *.go
# ./ts-dns -config ts-dns.ini
./ts-dns
```

## 配置说明


```ini
[main]
listen = :53
gfwlist = gfwlist.txt

[redis]

[servers]
clean = 119.29.29.29, 223.5.5.5
dirty = 8.8.8.8
work = 10.1.1.1

[rule:dirty]
suffix = google.com, twimg.com, quoracdn.net
socks5 = 127.0.0.1:1080

[rule:work]
suffix = company.com
```

* 配置文件默认为`ts-dns`。
* `gfwlist.txt`参考`https://github.com/gfwlist/gfwlist/raw/master/gfwlist.txt`。
* 反污染功能无法关闭。如不想使用该功能可将`servers`中的`clean`字段和`dirty`字段设为相同值。
* 当配置了`redis`Section里配置了`host`时，本程序将使用`Redis`作为分组判定结果缓存，反之则使用内置的`TTLMap`作为缓存。
* 污染判定机制尚不完善，目前已知无法正确判定是否被污染的域名如`rule:dirty`所示。推荐使用`GFWList`实现更好的准确度。
* `dirty`组DNS里的地址推荐设置为自建的`dnscrypt-proxy`，即搭配DOH/DOT使用。

## TODO

* HOSTS
* 自动添加IPSET
