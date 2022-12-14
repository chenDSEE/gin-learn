// Copyright 2013 Julien Schmidt. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be found
// at https://github.com/julienschmidt/httprouter/blob/master/LICENSE

package gin

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin/internal/bytesconv"
)

var (
	strColon = []byte(":")
	strStar  = []byte("*")
	strSlash = []byte("/")
)

// Param is a single URL parameter, consisting of a key and a value.
type Param struct {
	Key   string
	Value string
}

// Params is a Param-slice, as returned by the router.
// The slice is ordered, the first URL parameter is also the first slice value.
// It is therefore safe to read values by the index.
type Params []Param

// Get returns the value of the first Param which key matches the given name and a boolean true.
// If no matching Param is found, an empty string is returned and a boolean false .
func (ps Params) Get(name string) (string, bool) {
	for _, entry := range ps {
		if entry.Key == name {
			return entry.Value, true
		}
	}
	return "", false
}

// ByName returns the value of the first Param which key matches the given name.
// If no matching Param is found, an empty string is returned.
func (ps Params) ByName(name string) (va string) {
	va, _ = ps.Get(name)
	return
}

type methodTree struct {
	method string
	root   *node
}

type methodTrees []methodTree

func (trees methodTrees) get(method string) *node {
	for _, tree := range trees {
		if tree.method == method {
			return tree.root
		}
	}
	return nil
}

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func longestCommonPrefix(a, b string) int {
	i := 0
	max := min(len(a), len(b))
	for i < max && a[i] == b[i] {
		i++
	}
	return i
}

// addChild will add a child node, keeping wildcardChild at the end
func (n *node) addChild(child *node) {
	if n.wildChild && len(n.children) > 0 {
		// n.wildChild 就意味着当前的 node 有一个（最多也只有一个） wildcard 类型的 child node
		// 这个 node 总是排在最末尾
		// Q&A(DONE): 为什么 wildcard child 总是要在最末尾？
		// 因为 n.indices 没法子表达 wildcard 呀
		// 这其实是要解决静态路由跟动态路由的冲突问题，优先匹配静态路由
		// 实在不行才匹配动态路由
		// 不做这个处理的话，下面两个实际上是会发生冲突的：
		// GET /user/:id
		// GET /user/:id/post
		wildcardChild := n.children[len(n.children)-1]
		n.children = append(n.children[:len(n.children)-1], child, wildcardChild)
	} else {
		n.children = append(n.children, child)
	}
}

func countParams(path string) uint16 {
	var n uint16
	s := bytesconv.StringToBytes(path)
	n += uint16(bytes.Count(s, strColon))
	n += uint16(bytes.Count(s, strStar))
	return n
}

func countSections(path string) uint16 {
	s := bytesconv.StringToBytes(path)
	return uint16(bytes.Count(s, strSlash))
}

// Q&A: 这三种不同的 nodeType 有什么区别？
// 0 就意味着普通节点（默认情况）
// root 就是根节点
// param 则是 :param 的节点, 而且这个节点是不会混着其他 path 字符串的
//      （便于后续 param 的解析），同时 node 的 wildcard 也是被置位的
// catchAll 通用匹配，匹配任意参数(*user), 同时 node 的 wildcard 也是被置位的
type nodeType uint8

const (
	root nodeType = iota + 1
	param
	catchAll
)

// param, catchAll 这两种 type 的 node，wildChild 都是被设置为 true 的
type node struct {
	path      string // 自己这个 node 节点的部分 path
	// Q&A(DONE): indices 有什么用？
	// wildChild=true，参数节点时，indices=""
	// indices 实际上是记录了当前 node 所有 childNode.path[0]
	// 这样就可以加速 next node 的搜索
	// 而且这不是加入顺序的，是按照可能性来进行排序的
	// 当有更多的 URL 会路过这个 node 的时候，这些分支就会拍的更前面
	indices   string
	wildChild bool // wildcard child node 总是排在 children 的最末尾
	nType     nodeType
	priority  uint32  // 记录有多少的 URL 会途径这个 node

	// child nodes, at most 1 :param style node at the end of the array
	// 最多只会有一个 wildcard child，而且这个 node 总是在 children 的最末尾
	children  []*node
	handlers  HandlersChain
	fullPath  string // 完整的路径，从 root 到当前节点
}

// Increments priority of the given child and reorders if necessary
// 通过影响 indices，将更多 URL 经过的 node 放在前面
func (n *node) incrementChildPrio(pos int) int {
	cs := n.children // cs stand for 'child s'
	cs[pos].priority++
	prio := cs[pos].priority

	// Adjust position (move to front)
	newPos := pos
	for ; newPos > 0 && cs[newPos-1].priority < prio; newPos-- {
		// Swap node positions
		cs[newPos-1], cs[newPos] = cs[newPos], cs[newPos-1]
	}

	// Build new index char string
	if newPos != pos {
		n.indices = n.indices[:newPos] + // Unchanged prefix, might be empty
			n.indices[pos:pos+1] + // The index char we move
			n.indices[newPos:pos] + n.indices[pos+1:] // Rest without char at 'pos'
	}

	return newPos
}

// addRoute adds a node with the given handle to the path.
// Not concurrency-safe! 因为要注入 handlers 的缘故，也没有并发的必要了
// 也正是因为不支持热加载，所以是没有 delRoute() 的
func (n *node) addRoute(path string, handlers HandlersChain) {
	fullPath := path
	n.priority++

	// Empty tree
	// len(n.path) == 0 意味着当前 node 重来没有被填充过（是个新的 node）
	// len(n.children) == 0 意味着这个 node 也没有 children
	if len(n.path) == 0 && len(n.children) == 0 {
		n.insertChild(path, fullPath, handlers)
		n.nType = root
		return
	}

	parentFullPathIndex := 0

	// 循环解析：
	// 1. 在循环的过程中，path 在不断的缩短（已经存在 tree node 中的字符，将会被不停删除）
	// 2. 在循环的过程中，n 不断指向这一轮需要检查的节点，实际上是在 tree 中从根节点不断向下
walk:
	for {
		// Find the longest common prefix.
		// This also implies that the common prefix contains no ':' or '*'
		// since the existing key can't contain those chars.
		i := longestCommonPrefix(path, n.path) // 剩下的 path 有多少个当前节点时相同的？

		/* 节点分裂, 分裂完之后还是要走检查下一个 if-branch */
		// Split edge
		if i < len(n.path) {
			// 当前节点有一部分 n.path 跟 path 是重合的
			// 那么当前节点就需要一分为二：
			// 1. 相同的部分继续是 parent node；（n.path[:i]）
			// 2. 剩下的变成新的 child (n.path[i:])
			// 3. 而 path[i:] 也是一个节点
			//    3.1 继续匹配其他的 children node
			//    3.2 自己门户，创建一个新的 children node

			// 上面的第 2 点：n.path[i:] 后面的独立为一个新的节点
			child := node{
				path:      n.path[i:],
				wildChild: n.wildChild,
				// 因为原本的 child 全部跟着这个新的 node 走，所以全部 child 相关的都挂在这个 node 下面
				indices:   n.indices,
				children:  n.children,
				handlers:  n.handlers,
				priority:  n.priority - 1,
				fullPath:  n.fullPath,
			}

			// 上面的第 1 点：n.path[:i] 继续为本节点
			n.children = []*node{&child}
			// []byte for proper unicode char conversion, see #65
			// n.indices 记录了 child node 的 path[0], 所谓一个快速匹配的方法
			n.indices = bytesconv.BytesToString([]byte{n.path[i]})
			n.path = path[:i] // 将已经分裂出去的部分，去掉
			n.handlers = nil  // 现在当前节点只可能是一个中转站，所以是不可能有 handler 的，handler 全部给上面的节点
			n.wildChild = false
			n.fullPath = fullPath[:parentFullPathIndex+i]
		}
		/*
		else { i > len(n.path); i == len(n.path)
			1. i > len(n.path) 显然是直接路过这个 node，不对当前 node 做任何动作
			2. i == len(n.path) 同样也可能是路过这个 node，不对当前 node 造成任何影响
		}
		*/

		// Make new node a child of this node
		// case1: path 还有部分 path 没有落实\扫描到具体的 node 上，还需要继续进行遍历
		// case2: 没有 node 可以继续遍历了，剩下的 path 需要自立门户，自己独立成为一个 node
		if i < len(path) { // 3.1 继续匹配其他的 children node
			path = path[i:] // 前面的 path[:i] 已经落实到具体节点了，可以删除
			c := path[0]

			// '/' after param
			// 当 node 是 param node 的时候，node.indices 就是 ""，所以只能手动补充为这个 node 进行搜索
			if n.nType == param && c == '/' && len(n.children) == 1 {
				parentFullPathIndex += len(n.path)
				n = n.children[0] // 下一轮遍历
				n.priority++
				continue walk
			}

			// Check if a child with the next path byte exists
			// 通过遍历的方式找出下一个 node
			for i, max := 0, len(n.indices); i < max; i++ {
				if c == n.indices[i] {
					// 因为 n.indices[i] 实际上是 child 节点的搜索顺序，所以一旦 match 中 indices 那么下一个节点必然是 n.children[i]
					// 剩下的 path 往 n.children[i] 上面挂载
					parentFullPathIndex += len(n.path)
					i = n.incrementChildPrio(i) // 调整 indices 的优先级顺序
					n = n.children[i]  // 下一轮遍历

					// 因为一个字符必然只会出现一次，所以 match 到了就可以退出了
					continue walk
				}
			}

			// 剩下的 path 在已有的 children 里面都找不到，所以只能新建 node
			// 3.2 自己门户，创建一个新的 children node
			// Otherwise insert it
			if c != ':' && c != '*' && n.nType != catchAll {
				// 普通 node
				// []byte for proper unicode char conversion, see #65
				n.indices += bytesconv.BytesToString([]byte{c})
				child := &node{
					fullPath: fullPath,
				}
				n.addChild(child)
				n.incrementChildPrio(len(n.indices) - 1)
				n = child
				fmt.Println("cds 0 ---> ", child.fullPath)

			} else if n.wildChild {
				// 因为 gin 只接受一个 wildcard node，所以需要进行检查
				// 如果要接收多个 wildcard node 的话，就必须采用回溯的匹配方案了
				// inserting a wildcard node, need to check if it conflicts with the existing wildcard
				n = n.children[len(n.children)-1]
				n.priority++

				// Check if the wildcard matches
				if len(path) >= len(n.path) && n.path == path[:len(n.path)] &&
					// Adding a child to a catchAll is not possible
					n.nType != catchAll &&
					// Check for longer wildcard, e.g. :name and :names
					(len(n.path) >= len(path) || path[len(n.path)] == '/') {
					continue walk
				}

				// Wildcard conflict
				// wildcard node 将会发生混淆，所以需要直接 panic 告知使用者
				pathSeg := path
				if n.nType != catchAll {
					pathSeg = strings.SplitN(pathSeg, "/", 2)[0]
				}
				prefix := fullPath[:strings.Index(fullPath, pathSeg)] + n.path
				panic("'" + pathSeg +
					"' in new path '" + fullPath +
					"' conflicts with existing wildcard '" + n.path +
					"' in existing prefix '" + prefix +
					"'")
			}

			// 实际上是给 if c != ':' && c != '*' && n.nType != catchAll 中
			// 新建的普通 node 加入 handler
			n.insertChild(path, fullPath, handlers) // 这里只是利用了 n.insertChild() 最后面的三行，填充 handler 罢了，还不如自己手动填
			return
		}

		// Otherwise add handle to current node
		if n.handlers != nil {
			// 路由重复添加，直接 panic 告知使用者
			panic("handlers are already registered for path '" + fullPath + "'")
		}

		n.handlers = handlers
		n.fullPath = fullPath
		return
	}
}

// Search for a wildcard segment and check the name for invalid characters.
// Returns -1 as index, if no wildcard was found.
func findWildcard(path string) (wildcard string, i int, valid bool) {
	// Find start
	for start, c := range []byte(path) {
		// A wildcard starts with ':' (param) or '*' (catch-all)
		if c != ':' && c != '*' {
			continue
		}

		// Find end and check for invalid characters
		valid = true
		for end, c := range []byte(path[start+1:]) {
			switch c {
			case '/':
				// wildcard 的结尾找到了
				return path[start : start+1+end], start, valid
			case ':', '*':
				valid = false
			}
		}

		// wildcard 就是结尾
		return path[start:], start, valid
	}
	return "", -1, false
}

// 这个函数很有可能已经不用了，至少在 addRoute 里面是很难进来的
// 如果没有 wildcard 需要进行处理，那就跳到这个函数的最末尾，简单 insert handler 就好
// If no wildcard was found, simply insert the path and handle
// 这个函数命名时真的差，这个函数的目的是：
// 1. 有 wildcard 的话，则额外增加一个 wildcard node（path 还有 wildcard 的话，就不断分裂 node）
// 2. 没有的话，则直接填 handler 就好了
// 实际上这是为了应对后面很多个 wildcard 的情况
func (n *node) insertChild(path string, fullPath string, handlers HandlersChain) {
	for {
		// Find prefix until first wildcard
		wildcard, i, valid := findWildcard(path)
		if i < 0 { // No wildcard found
			break
		}

		// The wildcard name must only contain one ':' or '*' character
		if !valid {
			panic("only one wildcard per path segment is allowed, has: '" +
				wildcard + "' in path '" + fullPath + "'")
		}

		// check if the wildcard has a name
		if len(wildcard) < 2 {
			panic("wildcards must be named with a non-empty name in path '" + fullPath + "'")
		}

		if wildcard[0] == ':' { // param
			if i > 0 {
				// Insert prefix before the current wildcard
				n.path = path[:i]
				path = path[i:]
			}

			// param 类型的 node 是独立的，不会混着其他 path
			child := &node{
				nType:    param,
				path:     wildcard,
				fullPath: fullPath,
			}
			n.addChild(child)
			n.wildChild = true
			n = child
			n.priority++

			// if the path doesn't end with the wildcard, then there
			// will be another subpath starting with '/'
			if len(wildcard) < len(path) {
				path = path[len(wildcard):]

				child := &node{
					priority: 1,
					fullPath: fullPath,
				}
				n.addChild(child)
				n = child
				continue
			}

			// Otherwise we're done. Insert the handle in the new leaf
			n.handlers = handlers
			return
		}

		// catchAll
		if i+len(wildcard) != len(path) {
			panic("catch-all routes are only allowed at the end of the path in path '" + fullPath + "'")
		}

		if len(n.path) > 0 && n.path[len(n.path)-1] == '/' {
			pathSeg := strings.SplitN(n.children[0].path, "/", 2)[0]
			panic("catch-all wildcard '" + path +
				"' in new path '" + fullPath +
				"' conflicts with existing path segment '" + pathSeg +
				"' in existing prefix '" + n.path + pathSeg +
				"'")
		}

		// currently fixed width 1 for '/'
		i--
		if path[i] != '/' {
			panic("no / before catch-all in path '" + fullPath + "'")
		}

		n.path = path[:i]

		// First node: catchAll node with empty path
		child := &node{
			wildChild: true,
			nType:     catchAll,
			fullPath:  fullPath,
		}

		n.addChild(child)
		n.indices = string('/')
		n = child
		n.priority++

		// second node: node holding the variable
		child = &node{
			path:     path[i:],
			nType:    catchAll,
			handlers: handlers,
			priority: 1,
			fullPath: fullPath,
		}
		n.children = []*node{child}

		return
	}

	// If no wildcard was found, simply insert the path and handle
	n.path = path
	n.handlers = handlers
	n.fullPath = fullPath
}

// nodeValue holds return values of (*Node).getValue method
type nodeValue struct {
	handlers HandlersChain
	params   *Params
	tsr      bool
	fullPath string
}

type skippedNode struct {
	path        string
	node        *node
	paramsCount int16
}

// Returns the handle registered with the given path (key). The values of
// wildcards are saved to a map.
// If no handle can be found, a TSR (trailing slash redirect) recommendation is
// made if a handle exists with an extra (without the) trailing slash for the
// given path.
// 返回的是变量，而不是指针，也是通过 COW 的方式确保并发安全
// 这种 COW 的方式虽然是浪费了一点点的 CPU，但是他省去了读写锁（毕竟读写锁，即便是读锁，也是有性能消耗的）
// 真想极致无锁体验，那就学 Linux 内核那用，采用 RCU 来管理路由表
func (n *node) getValue(path string, params *Params, skippedNodes *[]skippedNode, unescape bool) (value nodeValue) {
	var globalParamsCount int16

walk: // Outer loop for walking the tree
	for {
		prefix := n.path
		if len(path) > len(prefix) {
			if path[:len(prefix)] == prefix {
				path = path[len(prefix):]

				// Try all the non-wildcard children first by matching the indices
				idxc := path[0]
				for i, c := range []byte(n.indices) {
					if c == idxc {
						//  strings.HasPrefix(n.children[len(n.children)-1].path, ":") == n.wildChild
						if n.wildChild {
							index := len(*skippedNodes)
							*skippedNodes = (*skippedNodes)[:index+1]
							(*skippedNodes)[index] = skippedNode{
								path: prefix + path,
								node: &node{
									path:      n.path,
									wildChild: n.wildChild,
									nType:     n.nType,
									priority:  n.priority,
									children:  n.children,
									handlers:  n.handlers,
									fullPath:  n.fullPath,
								},
								paramsCount: globalParamsCount,
							}
						}

						n = n.children[i]
						continue walk
					}
				}

				if !n.wildChild {
					// If the path at the end of the loop is not equal to '/' and the current node has no child nodes
					// the current node needs to roll back to last vaild skippedNode
					if path != "/" {
						for l := len(*skippedNodes); l > 0; {
							skippedNode := (*skippedNodes)[l-1]
							*skippedNodes = (*skippedNodes)[:l-1]
							if strings.HasSuffix(skippedNode.path, path) {
								path = skippedNode.path
								n = skippedNode.node
								if value.params != nil {
									*value.params = (*value.params)[:skippedNode.paramsCount]
								}
								globalParamsCount = skippedNode.paramsCount
								continue walk
							}
						}
					}

					// Nothing found.
					// We can recommend to redirect to the same URL without a
					// trailing slash if a leaf exists for that path.
					value.tsr = path == "/" && n.handlers != nil
					return
				}

				// Handle wildcard child, which is always at the end of the array
				n = n.children[len(n.children)-1]
				globalParamsCount++

				switch n.nType {
				case param:
					// fix truncate the parameter
					// tree_test.go  line: 204

					// Find param end (either '/' or path end)
					end := 0
					for end < len(path) && path[end] != '/' {
						end++
					}

					// Save param value
					if params != nil && cap(*params) > 0 {
						if value.params == nil {
							value.params = params
						}
						// Expand slice within preallocated capacity
						i := len(*value.params)
						*value.params = (*value.params)[:i+1]
						val := path[:end]
						if unescape {
							if v, err := url.QueryUnescape(val); err == nil {
								val = v
							}
						}
						(*value.params)[i] = Param{
							Key:   n.path[1:],
							Value: val,
						}
					}

					// we need to go deeper!
					if end < len(path) {
						if len(n.children) > 0 {
							path = path[end:]
							n = n.children[0]
							continue walk
						}

						// ... but we can't
						value.tsr = len(path) == end+1
						return
					}

					if value.handlers = n.handlers; value.handlers != nil {
						value.fullPath = n.fullPath
						return
					}
					if len(n.children) == 1 {
						// No handle found. Check if a handle for this path + a
						// trailing slash exists for TSR recommendation
						n = n.children[0]
						value.tsr = (n.path == "/" && n.handlers != nil) || (n.path == "" && n.indices == "/")
					}
					return

				case catchAll:
					// Save param value
					if params != nil {
						if value.params == nil {
							value.params = params
						}
						// Expand slice within preallocated capacity
						i := len(*value.params)
						*value.params = (*value.params)[:i+1]
						val := path
						if unescape {
							if v, err := url.QueryUnescape(path); err == nil {
								val = v
							}
						}
						(*value.params)[i] = Param{
							Key:   n.path[2:],
							Value: val,
						}
					}

					value.handlers = n.handlers
					value.fullPath = n.fullPath
					return

				default:
					panic("invalid node type")
				}
			}
		}

		if path == prefix {
			// If the current path does not equal '/' and the node does not have a registered handle and the most recently matched node has a child node
			// the current node needs to roll back to last vaild skippedNode
			if n.handlers == nil && path != "/" {
				for l := len(*skippedNodes); l > 0; {
					skippedNode := (*skippedNodes)[l-1]
					*skippedNodes = (*skippedNodes)[:l-1]
					if strings.HasSuffix(skippedNode.path, path) {
						path = skippedNode.path
						n = skippedNode.node
						if value.params != nil {
							*value.params = (*value.params)[:skippedNode.paramsCount]
						}
						globalParamsCount = skippedNode.paramsCount
						continue walk
					}
				}
				//	n = latestNode.children[len(latestNode.children)-1]
			}
			// We should have reached the node containing the handle.
			// Check if this node has a handle registered.
			if value.handlers = n.handlers; value.handlers != nil {
				value.fullPath = n.fullPath
				return
			}

			// If there is no handle for this route, but this route has a
			// wildcard child, there must be a handle for this path with an
			// additional trailing slash
			if path == "/" && n.wildChild && n.nType != root {
				value.tsr = true
				return
			}

			// No handle found. Check if a handle for this path + a
			// trailing slash exists for trailing slash recommendation
			for i, c := range []byte(n.indices) {
				if c == '/' {
					n = n.children[i]
					value.tsr = (len(n.path) == 1 && n.handlers != nil) ||
						(n.nType == catchAll && n.children[0].handlers != nil)
					return
				}
			}

			return
		}

		// Nothing found. We can recommend to redirect to the same URL with an
		// extra trailing slash if a leaf exists for that path
		value.tsr = path == "/" ||
			(len(prefix) == len(path)+1 && prefix[len(path)] == '/' &&
				path == prefix[:len(prefix)-1] && n.handlers != nil)

		// roll back to last valid skippedNode
		if !value.tsr && path != "/" {
			for l := len(*skippedNodes); l > 0; {
				skippedNode := (*skippedNodes)[l-1]
				*skippedNodes = (*skippedNodes)[:l-1]
				if strings.HasSuffix(skippedNode.path, path) {
					path = skippedNode.path
					n = skippedNode.node
					if value.params != nil {
						*value.params = (*value.params)[:skippedNode.paramsCount]
					}
					globalParamsCount = skippedNode.paramsCount
					continue walk
				}
			}
		}

		return
	}
}

// Makes a case-insensitive lookup of the given path and tries to find a handler.
// It can optionally also fix trailing slashes.
// It returns the case-corrected path and a bool indicating whether the lookup
// was successful.
func (n *node) findCaseInsensitivePath(path string, fixTrailingSlash bool) ([]byte, bool) {
	const stackBufSize = 128

	// Use a static sized buffer on the stack in the common case.
	// If the path is too long, allocate a buffer on the heap instead.
	buf := make([]byte, 0, stackBufSize)
	if length := len(path) + 1; length > stackBufSize {
		buf = make([]byte, 0, length)
	}

	ciPath := n.findCaseInsensitivePathRec(
		path,
		buf,       // Preallocate enough memory for new path
		[4]byte{}, // Empty rune buffer
		fixTrailingSlash,
	)

	return ciPath, ciPath != nil
}

// Shift bytes in array by n bytes left
func shiftNRuneBytes(rb [4]byte, n int) [4]byte {
	switch n {
	case 0:
		return rb
	case 1:
		return [4]byte{rb[1], rb[2], rb[3], 0}
	case 2:
		return [4]byte{rb[2], rb[3]}
	case 3:
		return [4]byte{rb[3]}
	default:
		return [4]byte{}
	}
}

// Recursive case-insensitive lookup function used by n.findCaseInsensitivePath
func (n *node) findCaseInsensitivePathRec(path string, ciPath []byte, rb [4]byte, fixTrailingSlash bool) []byte {
	npLen := len(n.path)

walk: // Outer loop for walking the tree
	for len(path) >= npLen && (npLen == 0 || strings.EqualFold(path[1:npLen], n.path[1:])) {
		// Add common prefix to result
		oldPath := path
		path = path[npLen:]
		ciPath = append(ciPath, n.path...)

		if len(path) == 0 {
			// We should have reached the node containing the handle.
			// Check if this node has a handle registered.
			if n.handlers != nil {
				return ciPath
			}

			// No handle found.
			// Try to fix the path by adding a trailing slash
			if fixTrailingSlash {
				for i, c := range []byte(n.indices) {
					if c == '/' {
						n = n.children[i]
						if (len(n.path) == 1 && n.handlers != nil) ||
							(n.nType == catchAll && n.children[0].handlers != nil) {
							return append(ciPath, '/')
						}
						return nil
					}
				}
			}
			return nil
		}

		// If this node does not have a wildcard (param or catchAll) child,
		// we can just look up the next child node and continue to walk down
		// the tree
		if !n.wildChild {
			// Skip rune bytes already processed
			rb = shiftNRuneBytes(rb, npLen)

			if rb[0] != 0 {
				// Old rune not finished
				idxc := rb[0]
				for i, c := range []byte(n.indices) {
					if c == idxc {
						// continue with child node
						n = n.children[i]
						npLen = len(n.path)
						continue walk
					}
				}
			} else {
				// Process a new rune
				var rv rune

				// Find rune start.
				// Runes are up to 4 byte long,
				// -4 would definitely be another rune.
				var off int
				for max := min(npLen, 3); off < max; off++ {
					if i := npLen - off; utf8.RuneStart(oldPath[i]) {
						// read rune from cached path
						rv, _ = utf8.DecodeRuneInString(oldPath[i:])
						break
					}
				}

				// Calculate lowercase bytes of current rune
				lo := unicode.ToLower(rv)
				utf8.EncodeRune(rb[:], lo)

				// Skip already processed bytes
				rb = shiftNRuneBytes(rb, off)

				idxc := rb[0]
				for i, c := range []byte(n.indices) {
					// Lowercase matches
					if c == idxc {
						// must use a recursive approach since both the
						// uppercase byte and the lowercase byte might exist
						// as an index
						if out := n.children[i].findCaseInsensitivePathRec(
							path, ciPath, rb, fixTrailingSlash,
						); out != nil {
							return out
						}
						break
					}
				}

				// If we found no match, the same for the uppercase rune,
				// if it differs
				if up := unicode.ToUpper(rv); up != lo {
					utf8.EncodeRune(rb[:], up)
					rb = shiftNRuneBytes(rb, off)

					idxc := rb[0]
					for i, c := range []byte(n.indices) {
						// Uppercase matches
						if c == idxc {
							// Continue with child node
							n = n.children[i]
							npLen = len(n.path)
							continue walk
						}
					}
				}
			}

			// Nothing found. We can recommend to redirect to the same URL
			// without a trailing slash if a leaf exists for that path
			if fixTrailingSlash && path == "/" && n.handlers != nil {
				return ciPath
			}
			return nil
		}

		n = n.children[0]
		switch n.nType {
		case param:
			// Find param end (either '/' or path end)
			end := 0
			for end < len(path) && path[end] != '/' {
				end++
			}

			// Add param value to case insensitive path
			ciPath = append(ciPath, path[:end]...)

			// We need to go deeper!
			if end < len(path) {
				if len(n.children) > 0 {
					// Continue with child node
					n = n.children[0]
					npLen = len(n.path)
					path = path[end:]
					continue
				}

				// ... but we can't
				if fixTrailingSlash && len(path) == end+1 {
					return ciPath
				}
				return nil
			}

			if n.handlers != nil {
				return ciPath
			}

			if fixTrailingSlash && len(n.children) == 1 {
				// No handle found. Check if a handle for this path + a
				// trailing slash exists
				n = n.children[0]
				if n.path == "/" && n.handlers != nil {
					return append(ciPath, '/')
				}
			}

			return nil

		case catchAll:
			return append(ciPath, path...)

		default:
			panic("invalid node type")
		}
	}

	// Nothing found.
	// Try to fix the path by adding / removing a trailing slash
	if fixTrailingSlash {
		if path == "/" {
			return ciPath
		}
		if len(path)+1 == npLen && n.path[len(path)] == '/' &&
			strings.EqualFold(path[1:], n.path[1:len(path)]) && n.handlers != nil {
			return append(ciPath, n.path...)
		}
	}
	return nil
}
