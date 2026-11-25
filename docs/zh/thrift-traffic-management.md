<!--
# Copyright Aeraki Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
-->

# Thrift 流量管理中的路由匹配

Aeraki 用 `VirtualService` 描述 Dubbo / Thrift 服务的路由规则，但这两个协议由不同的 Envoy 网络过滤器代理，
它们能表达的匹配条件并不相同。本文说明这种差异，以及 Thrift 一侧的转换规则。Dubbo 一侧的实现见
`internal/plugin/dubbo/route.go`，Thrift 一侧见 `internal/plugin/thrift/route.go`。

## 背景

早期的 Thrift 实现把每条规则的 `RouteMatch` 固定写死为“空方法名”，也就是匹配全部请求。由于 Envoy 按顺序
匹配路由、命中第一条即停止，只要 `VirtualService` 里写了多条带 `match` 的规则，第一条就会吞掉所有流量，
后面的规则和它们指向的子集永远走不到。下面的规则修正了这个问题。

## Envoy thrift_proxy 的匹配能力

`envoy.extensions.filters.network.thrift_proxy.v3.RouteMatch` 只有三项可用：

| 字段 | 含义 |
| --- | --- |
| `method_name` | 与请求方法名**精确**匹配；空字符串匹配任意方法 |
| `service_name` | 与服务名（方法名前缀）匹配；与 `method_name` **互斥** |
| `headers` | 一组 `HeaderMatcher`，支持 exact / prefix / regex 等 |

也就是说，Thrift 没有 Dubbo `MethodMatch.Name` 那样的前缀匹配和正则匹配。`service_name` 用于 Thrift
服务的多路复用（service multiplexing）场景，`VirtualService` 中没有对应的概念，Aeraki 不使用它。

## 与 Dubbo 的差异

| `VirtualService` 的匹配条件 | Dubbo `MethodMatch` / 头匹配 | Thrift `RouteMatch` |
| --- | --- | --- |
| `method.exact` | `MethodMatch.Name` 的 exact | `method_name` |
| `method.prefix` | `MethodMatch.Name` 的 prefix | **无对应表达**，丢弃该 match 元素并告警 |
| `method.regex` | `MethodMatch.Name` 的 safe_regex | **无对应表达**，丢弃该 match 元素并告警 |
| `headers.<name>.exact` | `HeaderMatcher` exact | `headers` 中的 `HeaderMatcher` exact |
| `headers.<name>.prefix` | `HeaderMatcher` prefix | `headers` 中的 `HeaderMatcher` prefix |
| `headers.<name>.regex` | `HeaderMatcher` safe_regex | `headers` 中的 `HeaderMatcher` safe_regex |
| `uri` / `authority` / `scheme` / `port` / `queryParams` / `sourceLabels` / `withoutHeaders` / `ignoreUriCase` | HTTP 专属概念，无意义 | 忽略并告警 |
| 多个 `match` 元素（逻辑或） | 只取第一个 | 每个元素展开为一条路由 |

## 转换规则

1. **保留规则顺序。** 路由表按 `VirtualService.Http` 中的顺序生成，Envoy 依次匹配。
2. **`method.exact` 映射为 `method_name`。**
3. **`method.prefix` / `method.regex` 丢弃该 `match` 元素并记录告警。** 之所以不做“忽略方法条件、只按头匹配”
   的降级，是因为那会把规则悄悄放大成匹配更多请求；之所以不整条 `VirtualService` 回退默认路由，是因为那会
   连带废掉同一份配置里其他本来可以生效的规则。丢弃单个元素的影响面最小，其余规则及其顺序保持不变。
4. **`headers` 的 exact / prefix / regex 映射为 `HeaderMatcher`**，按头名称排序生成，保证输出稳定。
5. **HTTP 专属字段忽略并告警。** 如果一个 `match` 元素只剩下这类字段（例如只写了 `uri` 和 `authority`），
   该元素没有任何 Thrift 能表达的匹配条件，直接丢弃并告警，**绝不**退化成匹配全部方法的路由。
6. **没有 `match` 的规则**，以及空的 `match` 元素（`- {}`），生成 `method_name: ""` 的路由，匹配全部方法，
   与旧行为一致。
7. **一个规则有多个 `match` 元素时逐个展开**，每个元素生成一条指向同一集群的路由。Istio 语义下这些元素
   本来就是“或”的关系，展开后语义一致。
8. **没有目的地的规则**（`http[].route` 为空）跳过并告警，避免越界访问。
9. **权重原样下发。** Thrift 的 `WeightedCluster` 没有 `total_weight` 字段，Envoy 以各集群权重之和作为总权重，
   所以不需要也不应该像 Dubbo 那样计算并填写总权重；权重和不为 100 时也按配置原样下发。
10. **没有任何规则能转换成功时**，仍生成一条 `method_name: ""` 的默认路由，指向不带子集的集群，避免出现
    空路由表导致所有请求失败。

## 示例

方法精确匹配的规则优先，无匹配条件的规则兜底：

```yaml
apiVersion: networking.istio.io/v1alpha3
kind: VirtualService
metadata:
  name: test-thrift-route
spec:
  hosts:
    - thrift-sample-server.thrift.svc.cluster.local
  http:
    - name: "get-user-v2"
      match:
        - method:
            exact: getUser
      route:
        - destination:
            host: thrift-sample-server.thrift.svc.cluster.local
            subset: v2
    - name: "default-v1"
      route:
        - destination:
            host: thrift-sample-server.thrift.svc.cluster.local
            subset: v1
```

生成的路由表依次为：`method_name: getUser` 指向 v2 的集群，`method_name: ""` 指向 v1 的集群，顺序不变。

如果第一条用的是前缀匹配：

```yaml
      match:
        - method:
            prefix: get
```

按第 3 条规则，该 `match` 元素被丢弃并记录告警，路由表中只剩第二条（`method_name: ""` 指向 v1）。如果整个
`VirtualService` 只有这一条前缀匹配的规则，则按第 10 条保留一条指向基础集群的默认路由。

## 兼容性

没有写 `match` 的存量 `VirtualService`，生成的路由配置与改造前逐字节一致。发生变化的是那些写了 `match` 的
配置：它们以前会退化成“第一条吞掉全部请求”，现在按上面的规则生成有效路由。
