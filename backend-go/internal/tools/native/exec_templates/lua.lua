-- exec's fn for lua, run before the program by lua -e. fn.name(...) calls the saved helper
-- functions_dir/lua/<name>.lua (fn["my-help"](...) for a dash); fn.name.member is any other
-- field. A helper file returns the helper: a function (its entry), or a table whose entry is call,
-- else the field named like the file (a dash written _), else its only function. The folder is
-- also on package.path, so helpers can require each other.
local dir = os.getenv("JAD_RUNNER_DIR")
if dir then
  package.path = dir .. "/?.lua;" .. package.path
end

local helpers = {}

local function entry(name, helper)
  for _, key in ipairs({ "call", (name:gsub("-", "_")) }) do
    if type(helper[key]) == "function" then
      return helper[key]
    end
  end
  local functions = {}
  for key, value in pairs(helper) do
    if type(value) == "function" then
      functions[#functions + 1] = tostring(key)
    end
  end
  table.sort(functions)
  if #functions == 1 then
    return helper[functions[1]]
  end
  error("fn." .. name .. " has no single entry to call ("
    .. (#functions > 0 and ("it has " .. table.concat(functions, ", ") .. "; call fn." .. name .. "." .. functions[1] .. "(...)") or "it has no function")
    .. "): add call or " .. name:gsub("-", "_"), 2)
end

local function load(name)
  if helpers[name] ~= nil then
    return helpers[name]
  end
  if type(name) ~= "string" or not name:match("^[A-Za-z0-9][A-Za-z0-9_-]*$") then
    error("fn." .. tostring(name) .. " is not a helper name: use letters, digits, _ and - and start with a letter or digit", 3)
  end
  local file = dir and (dir .. "/" .. name .. ".lua")
  local handle = file and io.open(file, "r")
  if not handle then
    error("no helper fn." .. name .. " in " .. (dir or "(the helper library does not exist)"), 3)
  end
  handle:close()
  local helper = dofile(file)
  if type(helper) == "table" then
    helper = setmetatable({}, {
      __index = helper,
      __call = function(_, ...)
        return entry(name, helper)(...)
      end,
    })
  elseif type(helper) ~= "function" then
    error("helper " .. file .. " must return a function or a table", 3)
  end
  helpers[name] = helper
  return helper
end

fn = setmetatable({}, {
  __index = function(_, name)
    return load(name)
  end,
})
