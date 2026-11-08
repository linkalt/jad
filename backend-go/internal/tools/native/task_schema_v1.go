package native

import "encoding/json"

// taskDescriptionV1 is the description native.task had before ops, which a tool
// definition file written then still holds (see requireTaskDefinition).
const taskDescriptionV1 = "Create and maintain session tasks."

// taskInputSchemaV1 is the schema native.task had before ops, offset, q and detail,
// with its three allOf rules. A tool definition file written then still holds it (see
// requireTaskDefinition); the model is shown taskInputSchema instead.
var taskInputSchemaV1 = json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["list","get","add","update","done","delete"],"description":"Optional when the supplied fields make the action clear."},
    "id":{"type":"string","minLength":1,"description":"Task id returned by add."},
    "title":{"type":"string","minLength":1,"description":"Task title for add or update."},
    "status":{"type":"string","enum":["todo","in-progress","blocked","done","all"],"description":"Task state; all is a list filter only."},
    "priority":{"type":"string","enum":["low","normal","high","urgent"],"description":"Optional task priority or list filter. Defaults to normal when adding."},
    "tags":{"type":"array","items":{"type":"string"},"description":"Task tags. An empty array clears tags on update."},
    "documents":{"type":"array","items":{"type":"string"},"description":"Planning-document paths. An empty array clears links on update."},
    "tag":{"type":"string","minLength":1,"description":"Optional tag filter for list."},
    "limit":{"type":"integer","minimum":1,"description":"Optional list limit; defaults to the session policy."}
  },
  "additionalProperties":false,
  "allOf":[
    {"if":{"properties":{"action":{"const":"add"}},"required":["action"]},"then":{"properties":{"title":{}},"required":["title"]}},
    {"if":{"properties":{"action":{"enum":["get","update","done","delete"]}},"required":["action"]},"then":{"properties":{"id":{}},"required":["id"]}},
    {"if":{"properties":{"action":{"const":"update"}},"required":["action"]},"then":{"anyOf":[
      {"properties":{"title":{}},"required":["title"]},
      {"properties":{"status":{}},"required":["status"]},
      {"properties":{"priority":{}},"required":["priority"]},
      {"properties":{"tags":{}},"required":["tags"]},
      {"properties":{"documents":{}},"required":["documents"]}
    ]}}
  ]
}`)
