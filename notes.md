most important Percentage of Successful Queries

```
(
      (
          max by (pod) (
            increase(
              avalanche_handler_messages{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",op="chits",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
            )
          )
        +
          1
      )
    /
      (
          (
              max by (pod) (
                increase(
                  avalanche_handler_messages{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",op="chits",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
                )
              )
            +
              max by (pod) (
                increase(
                  avalanche_handler_messages{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",op="query_failed",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
                )
              )
          )
        +
          1
      )
  )
*
  100
```

stake benched
```promql
max by (pod) (
  avg_over_time(
    avalanche_benchlist_benched_weight{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
  )
)
```

alert low stake connected
```
avg_over_time(max by (pod, namespace) (
    avalanche_stake_percent_connected{alerts="yes",is_subnet="true",pod_name!~"achilles.*",service="subnets",network="mainnet", chain="2FUHgWJcZ4j8FrEi1DsdGyq6vMWQXeQGbZLuzcU6sFAazvnrYd"}
)[5m:1m]) * 100
```


average block accept latency
```
max by (pod) (
  increase(
    avalanche_snowman_blks_accepted_sum{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
  )
)/max by (pod) (
  increase(
    avalanche_snowman_blks_accepted_count{availability_zone=~".*",chain=~"q2aTwKuyzgs8pynF7UXBZCU7DejbZbZ6EUyHr3JQzYgwNPUPi",component=~".*",environment=~".*",instance=~"(?i)(.*)",is_subnet=~"true",k8s_cluster_name=~"(?i)(.*)",network=~"mainnet",pod=~".*",region=~".*",service=~"subnets",version=~".*"}[5m]
  )
)/1000000000
```

last accepted height - yes
rejected blocks - yes
avg block accept latency
per of successful queries
stake non connected
stake benched
requests failed
avg verification time
block verification failed

system

networking metrics?

gas per block
ns per gas

evm tx pool something
