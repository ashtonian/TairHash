
## 命令介绍

#### EXHSET


语法及复杂度：


> EXHSET key field value [EX time] [EXAT time] [PX time] [PXAT time] [NX/XX] [VER/ABS/GT version] [KEEPTTL]  
> 时间复杂度：O(1)



命令描述：


> 向key指定的TairHash中插入一个field，如果TairHash不存在则自动创建一个，如果field已经存在则覆盖其值。在插入的时候，可以使用EX/EXAT/PX/PXAT给field设置过期时间，当field过期后会被主动（active expire）或被动（passivity expire）删除掉。如果指定了NX选项，则只有在field不存在的时候才会插入成功，同理，如果指定了XX选项，则只有在field存在的时候才能插入成功。如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以插入成功，如果field不存在或者field当前版本为0则不进行检查，总是可以插入成功。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以插入成功，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> value: TairHash中的一个元素对应的值  
> EX: 指定field的相对过期时间，单位为秒，0表示立刻过期
> EXAT: 指定field的绝对过期时间，单位为秒，0表示立刻过期
> PX: 指定field的相对过期时间，单位为毫秒，0表示立刻过期
> PXAT: 指定field的绝对过期时间，单位为毫秒 ，0表示立刻过期
> NX/XX: NX表示当要插入的field不存在的时候才允许插入，XX表示只有当field存在的时候才允许插入  
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0  
> KEEPTTL: 当未指定EX/EXAT/PX/PXAT时保留field的过期时间

返回值：


> 成功：新创建field并成功为它设置值时，命令返回1,如果field已经存在并且成功覆盖旧值，那么命令返回0 ；如果指定了XX且field不存在则返回-1，如果指定了NX且field已经存在返回-1；如果指定了VER且版本和当前版本不匹配则返回异常信息"ERR update version is stale"    
> 失败：返回相应异常信息  



#### EXHGET


语法及复杂度：


> EXHGET key field  
> 时间复杂度：O(1)



命令描述：


> 获取key指定的TairHash一个field的值，如果TairHash不存在或者field不存在，则返回nil  



参数：


> key: 用于查找该TairHash的键  
> field:   



返回值：


> 成功：当field存在时返回其对应的值，当TairHash不存在或者field不存在时返回nil  
> 失败：返回相应异常信息  



#### EXHMSET


语法及复杂度：


> EXHMSET key field value [field value...]    
> 时间复杂度：O(n)  



命令描述：


> 同时向key指定的TairHash中插入多个field，如果TairHash不存在则自动创建一个，如果field已经存在则覆盖其值  



参数：

 
> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> value: TairHash中的一个元素对应的值  



返回值：


> 成功：返回OK  
> 失败：返回相应异常信息  



#### EXHPEXPIREAT


语法及复杂度：


> EXHPEXPIREAT key field milliseconds-timestamp [VER/ABS/GT version]   
> 时间复杂度：O(1)  



命令描述：


> 在key指定的TairHash中为一个field设置绝对过期时间，单位为毫秒。当过期时间到时，该field会被主动删除。如果field不存在则直接返回0。如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以插入成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0



参数：


> key: 用于查找该TairHash的键    
> field: TairHash中的一个元素    
> milliseconds-timestamp: 以毫秒为单位的时间戳，0表示立刻过期   
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0


返回值：


> 成功：当field存在时返回1，当field不存在时返回0    
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息     



#### EXHPEXPIRE


语法及复杂度：


> EXHPEXPIRE key field milliseconds [VER/ABS/GT version]  
> 时间复杂度：O(1)  



命令描述：


> 在key指定的TairHash中为一个field设置相对过期时间，单位为毫秒。当过期时间到时，该field会被主动删除。如果field不存在则直接返回0。如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以插入成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0

参数：


> key: 用于查找该TairHash的键    
> field: TairHash中的一个元素    
> milliseconds: 以毫秒为单位的过期时间，0表示立刻过期       
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0


返回值：


> 成功：当field存在时返回1，当field不存在时返回0  
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息  



#### EXHEXPIREAT


语法及复杂度：


> EXHEXPIREAT key field timestamp [VER/ABS/GT version]  
> 时间复杂度：O(1)  



命令描述：


> 在key指定的TairHash中为一个field设置绝对过期时间，单位为秒，当过期时间到时，该field会被主动删除。如果field不存在则直接返回0。如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以插入成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0

参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> timestamp: 以秒为单位的时间戳，0表示立刻过期   
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0



返回值：


> 成功：当field存在时返回1，当field不存在时返回0  
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息  



#### EXHEXPIRE


语法及复杂度：


> EXHEXPIRE key field seconds [VER/ABS/GT version]  
> 时间复杂度：O(1)  



命令描述：


> 在key指定的TairHash中为一个field设置相对过期时间，单位为秒，当过期时间到时，该field会被主动删除。如果field不存在则直接返回0。如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以插入成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> seconds: 以秒为单位的过期时间，0表示立刻过期     
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0



返回值：


> 成功：当field存在时返回1，当field不存在时返回0  
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息  

#### EXHPERSIST


语法及复杂度：


> EXHEXPIRE key field
> 时间复杂度：O(1)  



命令描述：


> 将一个field设置为永不过期



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 1：成功移除field上的过期设置
> 0：key或field不存在，或field存在但当前没有过期设置



#### EXHPTTL


语法及复杂度：


> EXHPTTL key field    
> 时间复杂度：O(1)  



命令描述：


> 查看key指定的TairHash中一个field的剩余过期时间，单位为毫秒  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：当TairHash或者field不存在时返回-2，当field存在但是没有设置过期时间时返回-1，当field存在且设置过期时间时时返回对应过期时间，单位为毫秒  
> 失败：返回相应异常信息  



#### EXHTTL


语法及复杂度：


> EXHTTL key field    
> 时间复杂度：O(1)  



命令描述：


> 查看key指定的TairHash中一个field的剩余过期时间，单位为秒  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
 


返回值：


> 成功：当TairHash或者field不存在时返回-2，当field存在但是没有设置过期时间时返回-1，当field存在且设置过期时间时时返回对应过期时间，单位为秒  
> 失败：返回相应异常信息  



#### EXHVER


语法及复杂度：


> EXHVER key field     
> 时间复杂度：O(1)  



命令描述：


> 查看key指定的TairHash中一个field的当前版本号  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：当TairHash不存在时返回-1，当field不存在时返回-2，否则返回field版本号  
> 失败：返回相应异常信息  



#### EXHSETVER


语法及复杂度：


> EXHSETVER key field version    
> 时间复杂度：O(1)  



命令描述：


> 设置key指定的TairHash中一个field的版本号  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：当TairHash或者field不存在则返回0，否则返回1  
> 失败：返回相应异常信息  



#### EXHINCRBY


语法及复杂度：


> EXHINCRBY key field value [EX time] [EXAT time] [PX time] [PXAT time] [VER/ABS/GT version] [MIN minval] [MAX maxval] [KEEPTTL]    
> 时间复杂度：O(1)  



命令描述：


> 将key指定的TairHash中一个field的值加上整数value。如果TairHash不存在则自动新创建一个，如果指定的field不存在，则在加之前先将field的值初始化为0。同时还可以使用EX/EXAT/PX/PXAT为field设置过期时间。  
> 如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以设置成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0，MIN/MAX用户给field提供一个边界，只有本次incr操作后field的值还在此边界时incr才会被执行，否则返回overflow的错误



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> value: field对应的值  
> EX: 指定field的相对过期时间，单位为秒 ，0表示立刻过期
> EXAT: 指定field的绝对过期时间，单位为秒 ，0表示立刻过期
> PX: 指定field的相对过期时间，单位为毫秒，0表示立刻过期
> PXAT: 指定field的绝对过期时间，单位为毫秒，0表示立刻过期
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0
> MAX/MIN: 设置最大最小边界，本次incr操作后，field的值在此边界时incr才会被执行，否则返回overflow的错误。
> KEEPTTL: 当未指定EX/EXAT/PX/PXAT时保留field的过期时间

返回值：


> 成功：返回相加之后的值  
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息（如原来的field值不是浮点型）  



#### EXHINCRBYFLOAT


语法及复杂度：


> EXHINCRBYFLOAT key field value [EX time] [EXAT time] [PX time] [PXAT time] [VER/ABS/GT version] [MIN minval] [MAX maxval] [KEEPTTL]   
> 时间复杂度：O(1)  



命令描述：


> 将key指定的TairHash中一个field的值加上浮点型value。如果TairHash不存在则自动新创建一个，如果指定的field不存在，则在加之前先将field的值初始化为0。同时还可以使用EX/EXAT/PX/PXAT为field设置过期时间。  
> 如果指定了VER参数，则VER所携带的版本号必须和field当前版本号一致才可以设置成功，或者如果VER参数所携带的版本号为0，则不进行版本校验。ABS参数用于强制给field设置版本号，而不管field当前的版本号，总是可以设置成功，同时将field当前版本号设置为ABS指定的版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0，MIN/MAX用户给field提供一个边界，只有本次incr操作后field的值还在此边界时incr才会被执行，否则返回overflow的错误



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  
> value: field对应的值  
> EX: 指定field的相对过期时间，单位为秒，0表示立刻过期
> EXAT: 指定field的绝对过期时间，单位为秒，0表示立刻过期
> PX: 指定field的相对过期时间，单位为毫秒，0表示立刻过期
> PXAT: 指定field的绝对过期时间，单位为毫秒，0表示立刻过期
> VER/ABS/GT: VER表示只有指定的版本和field当前的版本一致时才允许设置，如果VER指定的版本为0则表示不进行版本检查，ABS表示无论field当前的版本是多少都强制设置并修改版本号，GT表示只有指定的版本大于当前版本时才允许设置，ABS和GT指定的版本号不能为0
> MAX/MIN: 设置最大最小边界，本次incr操作后，field的值在此边界时incr才会被执行，否则返回overflow的错误。
> KEEPTTL: 当未指定EX/EXAT/PX/PXAT时保留field的过期时间

返回值：


> 成功：返回相加之后的值  
> 失败：当版本校验失败时返回update version is stale错误，其他错误返回相应异常信息（如原来的field值不是浮点型）  



#### EXHGETWITHVER


语法及复杂度：


> EXHGETWITHVER key field    
> 时间复杂度：O(1)  



命令描述：


> 同时获取key指定的TairHash一个field的值和版本，如果TairHash不存在或者field不存在，则返回nil  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：如果TairHash不存在或者field不存在，则返回nil，否则返回field对应的值和版本  
> 失败：返回相应异常信息  



#### EXHMGET


语法及复杂度：


> EXHMGET key field [field ...]    
> 时间复杂度：O(n)  



命令描述：


> 同时获取key指定的TairHash多个field的值，如果TairHash不存在或者field不存在，则返回nil  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：返回一个数组，数组的每一个元素对应一个field, 如果TairHash不存在或者field不存在，则为nil，否则为field对应的值  
> 失败：返回相应异常信息  



#### EXHMGETWITHVER


语法及复杂度：


> EXHMGETWITHVER key field [field ...]    
> 时间复杂度：O(n)  



命令描述：


> 同时获取key指定的TairHash多个field的值和版本，如果TairHash不存在或者field不存在，则返回nil



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：返回一个数组，数组的每一个元素对应一个field, 如果TairHash不存在或者field不存在，则为nil，否则为field对应的值和版本  
> 失败：返回相应异常信息  



#### EXHDEL


语法及复杂度：


> EXHDEL key field [field...]   
> 时间复杂度：O(1)  



命令描述：


> 删除key指定的TairHash一个field，如果TairHash不存在或者field不存在则返回0 ，成功删除返回1



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：如果TairHash不存则返回0 ，成功怎返回成功删除的filed的个数  
> 失败：返回相应异常信息  



#### EXHLEN


语法及复杂度：


> EXHLEN key [noexp]   
> 时间复杂度：不是noexp选项时是O(1)，带noexp选项时是O(N)  



命令描述：


> 获取key指定的TairHash中field的个数，该命令默认不会触发对过期field的被动淘汰，也不会将其过滤掉，所以结果中可能包含已经过期但还未被删除的field。如果只想返回当前没有过期的field个数，那么可以最后带一个noexp参数，注意，带有该参数时，exhlen的RT将受到exhash大小的影响（因为要循环遍历），同时exhlen不会触发对field的淘汰，它只是把过期的field过滤了一下而已  



参数：


> key: 用于查找该TairHash的键  



返回值：


> 成功：如果TairHash不存在或者field不存在则返回0 ，成功删除返回TairHash中field个数  
> 失败：返回相应异常信息  



#### EXHEXISTS


语法及复杂度：


> EXHEXISTS key field  
> 时间复杂度：O(1)  



命令描述：


> 查询key指定的TairHash中是否存在对应的field  



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：如果TairHash不存在或者field不存在则返回0 ，如果field存在则返回1  
> 失败：返回相应异常信息



#### EXHSTRLEN


语法及复杂度：


> EXHSTRLEN key field   
> 时间复杂度：O(1)  



命令描述：


> 获取key指定的TairHash一个field的值的长度



参数：


> key: 用于查找该TairHash的键  
> field: TairHash中的一个元素  



返回值：


> 成功：如果TairHash不存在或者field不存在则返回0 ，否则返回field对应值的长度  
> 失败：返回相应异常信息  



#### EXHKEYS


语法及复杂度：


> EXHKEYS key  
> 时间复杂度：O(n)  



命令描述：


> 获取key指定的TairHash中所有field的键  



参数：


> key: 用于查找该TairHash的键  



返回值：


> 成功：返回一个数组，数组的每一位对应TairHash中的每一个field，如果TairHash不存则返回空的数组  
> 失败：返回相应异常信息  



#### EXHVALS


语法及复杂度：


> EXHVALS key  
> 时间复杂度：O(n)  



命令描述：


> 获取key指定的TairHash中所有field的值  



参数：


> key: 用于查找该TairHash的键  



返回值：


> 成功：返回一个数组，数组的每一位对应TairHash中的每一个field的值，如果TairHash不存则返回空的数组  
> 失败：返回相应异常信息  



#### EXHGETALL


语法及复杂度：


> EXHGETALL key  
> 时间复杂度：O(n)  



命令描述：


> 获取key指定的TairHash中所有field的键值对  



参数：


> key: 用于查找该TairHash的键



返回值：


> 成功：返回一个数组，数组的每一位对应TairHash中的每一个field的键值对，如果TairHash不存则返回空的数组  
> 失败：返回相应异常信息  



#### EXHGETALLWITHVER


语法及复杂度：


> EXHGETALLWITHVER key
> 时间复杂度：O(n)



命令描述：


> 获取key指定的TairHash中所有field的键值对和版本



参数：


> key: 用于查找该TairHash的键



返回值：


> 成功：返回一个数组，数组的每一位对应TairHash中的每一个field的键值对和版本，如果TairHash不存则返回空的数组
> 失败：返回相应异常信息



#### EXHSCAN


语法及复杂度：


> EXHSCAN key cursor [MATCH pattern] [COUNT count]   
> 时间复杂度：O(1)、O(N)  



命令描述：


> 扫描key指定的TairHash



参数：


> key: 用于查找该TairHash的键        
> cursor: 扫描的游标，从0开始，每次扫描后会返回下一次扫描的cursor，直到返回0表示扫描结束    
> MATCH: 用于对扫描结果进行过滤的规则      
> COUNT: 用于规定单次扫描field的个数，注意，COUNT仅表示每次扫描TairHash的feild的个数，不代表最终一定会返回COUNT个field结果集，结果集的大小还要根据TairHash中当前field个数和是否指定MATCH进行过滤而定。COUNT默认值为10     



返回值：


> 成功：返回一个具有两个元素的数组，数组第一个元素是下一次扫描需要使用的cursor，为0表示整个扫描结束。第二个数组元素还是一个数组，数组包含了所有本次被迭代的field/value。如果扫描到一个空的TairHash或者是TairHash不存在，那么这两个数组元素都为空。      
> 失败：返回相应异常信息  



**使用示例：**
1、如何使用渐进式扫描整个TairHash:   
```
127.0.0.1:6379> exhmset exhashkey field1 val1 field2 val2 field3 val3 field4 val4 field5 val5 field6 val6 field7 val7 field8 val8 field9 val9
OK
127.0.0.1:6379> exhscan exhashkey 0 COUNT 3
1) (integer) 4
2) 1) "field6"
   2) "val6"
   3) "field5"
   4) "val5"
127.0.0.1:6379> exhscan exhashkey 4 COUNT 3
1) (integer) 1
2) 1) "field8"
   2) "val8"
   3) "field2"
   4) "val2"
127.0.0.1:6379> exhscan exhashkey 1 COUNT 3
1) (integer) 13
2) 1) "field9"
   2) "val9"
   3) "field7"
   4) "val7"
127.0.0.1:6379> exhscan exhashkey 13 COUNT 3
1) (integer) 11
2) 1) "field3"
   2) "val3"
   3) "field4"
   4) "val4"
127.0.0.1:6379> exhscan exhashkey 11 COUNT 3
1) (integer) 0
2) 1) "field1"
   2) "val1"
```

2、如何使用MATCH对结果集进行过滤 
精确匹配：


```
127.0.0.1:6379> exhmset exhashkey field1_1 val1_1 field1_2 val1_2 field1_3 val1_3 field1_4 val1_4 field1_5 val1_5 field2_1 val2_1 field2_2 val2_2 field2_3 val2_3 field6_1 val6_1 field6_2 val6_2 field6_3 val6_3 field6_4 val6_4 field8_1 val8_1 field8_2 val8_4
OK
127.0.0.1:6379> exhscan exhashkey 0 COUNT 3 MATCH field1_1
1) (integer) 8
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 8 COUNT 3 MATCH field1_1
1) (integer) 12
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 12 COUNT 3 MATCH field1_1
1) (integer) 9
2) 1) "field1_1"
   2) "val1_1"
127.0.0.1:6379> exhscan exhashkey 9 COUNT 3 MATCH field1_1
1) (integer) 5
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 5 COUNT 3 MATCH field1_1
1) (integer) 11
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 11 COUNT 3 MATCH field1_1
1) (integer) 0
2) (empty array)
```


模糊匹配：

```
127.0.0.1:6379> exhmset exhashkey field1_1 val1_1 field1_2 val1_2 field1_3 val1_3 field1_4 val1_4 field1_5 val1_5 field2_1 val2_1 field2_2 val2_2 field2_3 val2_3 field6_1 val6_1 field6_2 val6_2 field6_3 val6_3 field6_4 val6_4 field8_1 val8_1 field8_2 val8_4
OK
127.0.0.1:6379> exhscan exhashkey 0 COUNT 3 MATCH field6_*
1) (integer) 8
2) 1) "field6_4"
   2) "val6_4"
   3) "field6_1"
   4) "val6_1"
127.0.0.1:6379> exhscan exhashkey 8 COUNT 3 MATCH field6_*
1) (integer) 12
2) 1) "field6_2"
   2) "val6_2"
127.0.0.1:6379> exhscan exhashkey 12 COUNT 3 MATCH field6_*
1) (integer) 9
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 9 COUNT 3 MATCH field6_*
1) (integer) 5
2) (empty array)
127.0.0.1:6379> exhscan exhashkey 5 COUNT 3 MATCH field6_*
1) (integer) 11
2) 1) "field6_3"
   2) "val6_3"
127.0.0.1:6379> exhscan exhashkey 11 COUNT 3 MATCH field6_*
1) (integer) 0
2) (empty array)
```

<br/>

## Last-write-wins写入与数据源同步

以下命令用于把TairHash作为外部数据源（例如保存每个field最新值的数据库表）的缓存，并且不会返回不完整的数据：

1. 写入方使用 `EXHMSETLWW` 写入每次变更，以数据源的写入时间戳作为版本。被更新的写入覆盖是预期结果，不是错误。
2. 读取方调用 `EXHSYNCGET`。从未同步过的TairHash（新建、被淘汰或重启后丢失）视为未命中：读取方从数据源读取全部field，连同 `EXHSYNCGET` 返回的token一起交给 `EXHSYNC`。
3. `EXHSYNC` 合并快照，删除数据源中已不存在的field，并把TairHash标记为已同步。在token之后写入的field与快照读取存在竞争，无论版本如何都会保留。

所有写入value的命令都会为field记录一个由服务端分配的写入序号。序号严格递增，会被复制和持久化，并且总是大于之前的进程或故障切换前的主节点分配的序号，因此token在key删除、重启和故障切换后依然有效。

#### EXHMSETLWW


语法及复杂度：


> EXHMSETLWW key field value version [field value version ...]
> 时间复杂度：O(N)，N为field个数



命令描述：


> 以last-write-wins语义写入key指定的TairHash中的field，TairHash不存在时自动创建。当field不存在、版本更高，或版本相同且value按字节比较更大（一个value是另一个的前缀时，较短的更小）时写入生效。版本相同时的比较规则与Cassandra/Scylla相同，因此结果与写入到达的顺序无关。生效的写入会替换value和版本，并清除field的过期时间。field按参数顺序写入。



参数：


> key: 用于查找该TairHash的键
> field: TairHash中的一个元素
> value: 写入的值
> version: 写入的版本，正整数（例如数据源的微秒级写入时间戳）



返回值：


> 成功：返回数组，每个field对应一个整数，1表示写入生效，0表示field已有更新（或比较更大）的写入
> 失败：版本不是正整数时返回syntax error（不会写入任何数据）；key为其他类型时返回WRONGTYPE



#### EXHSYNCGET


语法及复杂度：


> EXHSYNCGET key [field ...]
> 时间复杂度：O(N)，N为TairHash中的field个数或指定的field个数



命令描述：


> 如果key指定的TairHash已同步（见EXHSYNC），返回其内容以及同步token。TairHash不存在或从未同步时视为未命中：不返回field，调用方应在此调用之后从数据源读取快照，并把token交给EXHSYNC。指定field时只返回存在的field，顺序与请求一致；对已同步的TairHash，field不存在表示数据源中没有该field。已过期的field不会返回。



参数：


> key: 用于查找该TairHash的键
> field: 要返回的field（默认返回全部）



返回值：


> 成功：返回三个元素的数组：已同步为1否则为0；同步token（整数）；field、value、version三元组组成的数组（未命中时为空）
> 失败：key为其他类型时返回WRONGTYPE



#### EXHSYNC


语法及复杂度：


> EXHSYNC key token [field value version ...]
> 时间复杂度：O(N + M)，N为快照中的field个数，M为TairHash中的field个数



命令描述：


> 用数据源的完整快照（在EXHSYNCGET返回token之后读取）校正key指定的TairHash：快照中的field按EXHMSETLWW语义合并；不在快照中的field会被删除，除非它们在token之后写入；最后把TairHash标记为已同步。已同步的TairHash即使没有field也会保留，因此空快照可以缓存“数据源中没有该key的数据”。已同步状态在key被删除之前一直保持。



参数：


> key: 用于查找该TairHash的键
> token: 读取快照之前EXHSYNCGET返回的token（0表示不删除调用之前写入的任何field）
> field value version: 快照内容，版本为正整数



返回值：


> 成功：返回两个整数的数组：写入生效的快照field个数，以及删除的field个数
> 失败：token为负数或版本不是正整数时返回syntax error（不做任何修改）；key为其他类型时返回WRONGTYPE

<br/>

## 复制与持久化

写命令把本次写入的field的结果状态（而不是原始请求）合并为一条 `EXHMSETREPL key field value version expire seq [field value version expire seq ...]` 进行复制（expire为毫秒级unix时间戳，0表示不过期），因此从节点或AOF重放可以精确还原版本、过期时间和写入序号，与其自身时钟无关。EXHSYNC还会为删除的field发送 `EXHDEL`，并发送 `EXHSYNCREPL key` 标记已同步。AOF重写输出同样的命令，每条 `EXHMSETREPL` 最多64个field。`EXHMSETREPL` 与 `EXHSYNCREPL` 是内部命令，只接受来自复制流或加载过程的调用，普通客户端会收到 `ERR only accepted from the replication stream or while loading`（Redis 6.2之前的版本无法判断命令来源，因此总是接受）。

RDB编码版本为1（增加已同步标记与每个field的写入序号）。版本0的数据加载后为未同步状态，写入序号为0。此版本写入的数据无法被旧版本加载，旧版本的从节点不认识 `EXHMSETREPL`：请先升级从节点再升级主节点。
