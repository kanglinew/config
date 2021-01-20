// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package config

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"go.uber.org/config/internal/merge"
	"go.uber.org/config/internal/unreachable"
	yaml "gopkg.in/yaml.v2"
)

const _separator = "."

//YAML是从一个或多个YAML源读取的提供者。
//结果提供者行为的许多方面可以通过传递函数选项来改变。
//默认情况下，YAML提供者尝试主动捕捉常见错误
//通过启用gopkg.in/yaml公司.v2的严格模式。
//有关详细信息，请参阅关于严格解组的包级文档。
//填充Go结构时，YAML提供程序正确生成的值
type YAML struct {
	name     string
	raw      [][]byte
	lookup   LookupFunc // see withDefault
	contents interface{}
	strict   bool
	empty    bool
}


//NewYAML构造一个YAML提供者。
//有关默认行为的可用调整，请参见各种YAMLOptions。
func NewYAML(options ...YAMLOption) (*YAML, error) {
	cfg := &config{
		strict: true,
		name:   "YAML",
	}
	for _, o := range options {
		o.apply(cfg)
	}

	if cfg.err != nil {
		return nil, fmt.Errorf("error applying options: %v", cfg.err)
	}
	//有些源不应该扩展环境变量；通过转义内容来保护这些源。
	//（合并前扩展会重新暴露出许多错误，因此我们不能在合并前选择性地扩展源代码。）
	sourceBytes := make([][]byte, len(cfg.sources))
	for i := range cfg.sources {
		s := cfg.sources[i]
		if !s.raw {
			sourceBytes[i] = s.bytes
			continue
		}
		sourceBytes[i] = escapeVariables(s.bytes)
	}

	//在构造时，经历一个完整的merge-serialize-deserialize循环，以尽早捕获任何重复的键（在严格模式下）。
	//它还剥离了注释，从而阻止我们尝试环境变量扩展。（接下来我们将展开环境变量。）
	merged, err := merge.YAML(sourceBytes, cfg.strict)
	if err != nil {
		return nil, fmt.Errorf("couldn't merge YAML sources: %v", err)
	}

	// Expand environment variables.
	merged, err = expandVariables(cfg.lookup, merged)
	if err != nil {
		return nil, err
	}

	y := &YAML{
		name:   cfg.name,
		raw:    sourceBytes,
		lookup: cfg.lookup,
		strict: cfg.strict,
	}

	dec := yaml.NewDecoder(merged)
	dec.SetStrict(cfg.strict)
	if err := dec.Decode(&y.contents); err != nil {
		if err != io.EOF {
			return nil, fmt.Errorf("couldn't decode merged YAML: %v", err)
		}
		y.empty = true
	}

	return y, nil
}

//Name返回提供程序的名称。默认为“YAML”。
func (y *YAML) Name() string {
	return y.name
}

//Get从配置中检索值。提供的键被视为一个以周期分隔的路径，每个路径段都用作映射键。
//例如，如果提供程序包含YAML
//   foo:
//     bar:
//       baz: hello
// then Get("foo.bar") returns a value holding
//   baz: hello
//
//要获取包含整个配置的值，请使用根常量作为键。
func (y *YAML) Get(key string) Value {
	return y.get(strings.Split(key, _separator))
}

func (y *YAML) get(path []string) Value {
	if len(path) == 1 && path[0] == Root {
		path = nil
	}
	return Value{
		path:     path,
		provider: y,
	}
}

//at返回给定路径上值的未编组表示形式，并用bool指示是否找到该值。
//
//YAML映射被解组为map[interface{}]interface{}，序列被解组为[]interface{}，标量被解组为interface{}。
func (y *YAML) at(path []string) (interface{}, bool) {
	if y.empty {
		return nil, false
	}

	cur := y.contents
	for _, segment := range path {
		//转换为映射类型。如果这失败了，那么我们就得到了一条不以序列或标量终止的路径。
		m, ok := cur.(map[interface{}]interface{})
		if !ok {
			return nil, false
		}

		//尝试将段解析为字符串，然后为可比较的键解组路径段。
		//毕竟，YAML标量类型不仅仅是字符串（boolean、integer等）。我们希望使用字符串形式来解析不明确的路径。
		if _, ok := m[segment]; !ok {
			var key interface{}
			if err := yaml.Unmarshal([]byte(segment), &key); err != nil {
				return nil, false
			}
			if !merge.IsScalar(key) {
				return nil, false
			}
			if _, ok := m[key]; !ok {
				return nil, false
			}
			cur = m[key]
		} else {
			cur = m[segment]
		}
	}
	return cur, true
}

func (y *YAML) populate(path []string, i interface{}) error {
	val, ok := y.at(path)
	if !ok {
		return nil
	}
	buf := &bytes.Buffer{}
	if err := yaml.NewEncoder(buf).Encode(val); err != nil {
		//提供者内容是由解编YAML生成的，这是不可能的。
		err := fmt.Errorf(
			"couldn't marshal config at key %s to YAML: %v",
			strings.Join(path, _separator),
			err,
		)
		return unreachable.Wrap(err)
	}
	dec := yaml.NewDecoder(buf)
	dec.SetStrict(y.strict)
	//解码永远不能返回EOF，因为编码任何值都保证生成非空YAML。
	return dec.Decode(i)
}

func (y *YAML) withDefault(d interface{}) (*YAML, error) {
	rawDefault := &bytes.Buffer{}
	if err := yaml.NewEncoder(rawDefault).Encode(d); err != nil {
		return nil, fmt.Errorf("can't marshal default to YAML: %v", err)
	}

	//在最初配置

//提供程序只不过是一个顶级null，但更高优先级的源包含一些额外的数据。
//在这种情况下，合并所有源的结果是非空的。但是，显式空源应该覆盖withDefault提供的所有数据。
//为了正确地处理这个问题，我们必须使用新的默认值作为最低优先级的源，并重新合并原始源。
	opts := []YAMLOption{
		Name(y.name),
		Expand(y.lookup),
		Source(rawDefault),
		//raw包含原始源，并对RawSources进行转义soappendsources不会对其进行双重扩展。
		appendSources(y.raw),
	}
	if !y.strict {
		opts = append(opts, Permissive())
	}
	return NewYAML(opts...)
}

//值是提供者配置的子集。
type Value struct {
	path     []string
	provider *YAML
}

//NewValue是一个非常容易出错的构造函数，仅为向后兼容而保留。
//如果value和found与提供的键处的提供程序的内容不匹配，它将崩溃。
//不推荐使用：这个内部构造函数在这个包的初始版本中被错误地导出，但是它的行为常常非常令人惊讶。
//为了在不改变函数签名的情况下保证行为正常，在版本1.2中添加了输入验证和panics。
//在所有情况下，使用它既安全又不冗长提供者。获取直接。
func NewValue(p Provider, key string, value interface{}, found bool) Value {
	actual := p.Get(key)
	if has := actual.HasValue(); has != found {
		var tmpl string
		if has {
			tmpl = "inconsistent parameters: provider %s has value at key %q but found parameter was false"
		} else {
			tmpl = "inconsistent parameters: provider %s has no value at key %q but found parameter was true"
		}
		panic(fmt.Sprintf(tmpl, p.Name(), key))
	}
	contents := actual.Value()
	same, err := areSameYAML(contents, value)
	if err != nil {
		panic(fmt.Sprintf("can't check NewValue parameter consistency: %v", err))
	}
	if !same {
		tmpl := "inconsistent parameters: provider %s has %#v at key %q but value was %#v"
		panic(fmt.Sprintf(tmpl, p.Name(), contents, key, value))
	}
	return actual
}

//Source返回值提供程序的名称。
func (v Value) Source() string {
	return v.provider.Name()
}


//Populate将值解组到目标结构中，与json.Unmarshal文件或者yaml.解组. 
//当用一些已经设置的字段填充结构时，数据将按照包级别中的描述进行深度合并文档。
func (v Value) Populate(target interface{}) error {
	return v.provider.populate(v.path, target)
}


//进一步深入到配置中，提取更深入的嵌套值。
//提供的路径按句点拆分，并且每个段都被视为嵌套的映射键。例如，如果当前值包含YAML配置
//   foo:
//     bar:
//       baz: quux
// then a call to Get("foo.bar") will hold the YAML mapping
//   baz: quux
func (v Value) Get(path string) Value {
	if path == Root {
		return v
	}
	extended := make([]string, len(v.path))
	copy(extended, v.path)
	extended = append(extended, strings.Split(path, _separator)...)
	return v.provider.get(extended)
}

//HasValue检查此键是否有任何配置可用。

//它不区分在提供程序构造期间提供的配置和默认应用的配置。
//如果该值已显式设置为nil，则HasValue为true。
//不赞成：这个函数没有什么价值，而且常常令人困惑。与其检查值是否有任何可用的配置，不如用适当的默认值和零值填充结构。
func (v Value) HasValue() bool {
	_, ok := v.provider.at(v.path)
	return ok
}

func (v Value) String() string {
	return fmt.Sprint(v.Value())
}

//值将配置解组到接口{}。

//不推荐：在强类型语言中，将配置解组到接口{}中是没有帮助的。使用强类型结构填充更安全、更容易。
func (v Value) Value() interface{} {
	//确保调用者不能改变配置的最简单方法是使用Populate进行深度复制。
	var i interface{}
	if err := v.Populate(&i); err != nil {
		//无法访问，因为我们已经确保了底层YAML是有效的。
		//在不破坏向后兼容性的情况下，无法更改此签名以包含错误。
		panic(unreachable.Wrap(err).Error())
	}
	return i
}

//WithDefault为值提供默认配置。默认值被序列化为YAML，然后使用包级文档中描述的合并逻辑将现有配置源深度合并到其中。
//ni请注意，应用默认值需要重新扩展环境变量，如果在提供程序构造之后环境发生更改，则可能会产生意外的结果。

//已弃用：WithDefault的深度合并行为非常复杂，尤其是在多次应用时。相反，创建一个Go结构，直接在结构上设置任何默认值，然后调用Populate。
func (v Value) WithDefault(d interface{}) (Value, error) {
	fallback := d
	for i := len(v.path) - 1; i >= 0; i-- {
		fallback = map[string]interface{}{v.path[i]: fallback}
	}
	p, err := v.provider.withDefault(fallback)
	if err != nil {
		return Value{}, err
	}
	return Value{path: v.path, provider: p}, nil
}
