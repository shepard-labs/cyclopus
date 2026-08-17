#!/usr/bin/env ruby
# frozen_string_literal: true

require 'yaml'

PLUGINS = {
  'buf.build/protocolbuffers/go' => ['protoc-gen-go', 'google.golang.org/protobuf/cmd/protoc-gen-go'],
  'buf.build/grpc/go' => ['protoc-gen-go-grpc', 'google.golang.org/grpc/cmd/protoc-gen-go-grpc'],
  'buf.build/grpc-ecosystem/gateway' => ['protoc-gen-grpc-gateway',
                                         'github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway'],
  'buf.build/grpc-ecosystem/openapiv2' => ['protoc-gen-openapiv2',
                                           'github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-openapiv2']
}.freeze
TOP_LEVEL_FIELDS = %w[version clean plugins].freeze
PLUGIN_FIELDS = %w[remote out opt strategy include_imports include_wkt].freeze
SEMVER = /\A(?:v)?(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\z/.freeze

def fail_config(message)
  warn "derive local Buf template: #{message}"
  exit 1
end

def node_fingerprint(node)
  case node
  when Psych::Nodes::Scalar
    ['scalar', node.value]
  when Psych::Nodes::Sequence
    ['sequence', *node.children.map { |child| node_fingerprint(child) }]
  when Psych::Nodes::Mapping
    ['mapping', *node.children.map { |child| node_fingerprint(child) }]
  when Psych::Nodes::Alias
    ['alias', node.anchor]
  else
    [node.class.name]
  end
end

def reject_duplicate_mapping_keys(node, path = 'top level')
  if node.is_a?(Psych::Nodes::Mapping)
    seen = {}
    node.children.each_slice(2).with_index do |(key, value), index|
      fingerprint = node_fingerprint(key)
      if seen[fingerprint]
        fail_config("duplicate mapping key at #{path}: #{key.respond_to?(:value) ? key.value : index + 1}")
      end

      seen[fingerprint] = true
      reject_duplicate_mapping_keys(key, "#{path} key #{index + 1}")
      reject_duplicate_mapping_keys(value, "#{path}.#{key.respond_to?(:value) ? key.value : index + 1}")
    end
  elsif node.respond_to?(:children) && node.children
    node.children.each_with_index do |child, index|
      reject_duplicate_mapping_keys(child, "#{path}[#{index}]")
    end
  end
end

source, template_path, installs_path = ARGV
fail_config('usage: derive_buf_gen_local.rb SOURCE TEMPLATE INSTALLS') unless installs_path

begin
  syntax_tree = Psych.parse_file(source)
  reject_duplicate_mapping_keys(syntax_tree)
  config = YAML.safe_load_file(source, permitted_classes: [], permitted_symbols: [], aliases: false)
rescue Psych::Exception, SystemCallError => e
  fail_config("cannot parse #{source}: #{e.message}")
end

fail_config('top level must be a mapping') unless config.is_a?(Hash)
unknown = config.keys - TOP_LEVEL_FIELDS
fail_config("unsupported top-level field(s): #{unknown.join(', ')}") unless unknown.empty?
fail_config('version must be v2') unless config['version'] == 'v2'
fail_config('clean must be true or false') unless [true, false].include?(config['clean'])
plugins = config['plugins']
fail_config('plugins must be a non-empty sequence') unless plugins.is_a?(Array) && !plugins.empty?

seen = {}
installs = []
local_plugins = plugins.each_with_index.map do |plugin, index|
  label = "plugin #{index + 1}"
  fail_config("#{label} must be a mapping") unless plugin.is_a?(Hash)
  unknown = plugin.keys - PLUGIN_FIELDS
  fail_config("#{label} has unsupported field(s): #{unknown.join(', ')}") unless unknown.empty?

  remote = plugin['remote']
  fail_config("#{label} is missing remote") unless remote.is_a?(String) && !remote.empty?
  match = remote.match(/\A([^:]+):([^:]+)\z/)
  fail_config("#{label} remote must include exactly one version: #{remote}") unless match
  identity = match[1]
  version = match[2]
  fail_config("#{label} remote has invalid version: #{version.inspect}") unless SEMVER.match?(version)
  fail_config("unknown remote plugin: #{identity}") unless PLUGINS.key?(identity)
  fail_config("duplicate remote plugin: #{identity}") if seen[identity]
  seen[identity] = true

  out = plugin['out']
  fail_config("#{label} out must be a non-empty string") unless out.is_a?(String) && !out.empty?
  if plugin.key?('opt') && (!plugin['opt'].is_a?(Array) || !plugin['opt'].all? { |option| option.is_a?(String) })
    fail_config("#{label} opt must be a sequence of strings")
  end
  %w[strategy].each do |field|
    fail_config("#{label} #{field} must be a string") if plugin.key?(field) && !plugin[field].is_a?(String)
  end
  %w[include_imports include_wkt].each do |field|
    fail_config("#{label} #{field} must be true or false") if plugin.key?(field) && ![true,
                                                                                      false].include?(plugin[field])
  end

  executable, install_module = PLUGINS.fetch(identity)
  installs << [install_module, "v#{version.delete_prefix('v')}"]
  plugin.each_with_object({}) do |(field, value), local|
    local[field == 'remote' ? 'local' : field] = field == 'remote' ? executable : value
  end
end

local_config = config.merge('plugins' => local_plugins)
File.write(template_path, YAML.dump(local_config))
File.write(installs_path, installs.map { |mod, version| "#{mod}\t#{version}\n" }.join)
