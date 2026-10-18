#!/usr/bin/env bash
# gateway/deploy/eval/persona_body.sh — the request body for one origination route,
# built from the network's discovery descriptor: the route's own keys plus the persona
# the descriptor states for it (member, order system and code, claim facts), and — for
# a route that sends an amendment — the amendment's certificationType.
#
# usage: persona_body.sh <descriptor.json> <scenario> <lane> <amend:0|1> [<route-keys-json>]
#   scenario: the descriptor's scenario key ("uc04", "uc01-notcovered", "uc05-noconsent").
#
# It states none of the facts itself: a scenario the descriptor states no persona for, or
# a persona order missing its claim type, priority or service category, is refused (exit 1)
# and nothing is printed. An amendment's certificationType is the persona's own
# statement (its descriptor entry's amendment.certificationType); a persona that states
# none is refused the same way.
set -euo pipefail
desc=$1 scenario=$2 lane=$3 amend=$4 keys=${5:-'{}'}
jq -ce --arg s "$scenario" --arg l "$lane" --argjson amend "$amend" --argjson keys "$keys" '
  [(.scenarioPersonas // [])[] | select(.scenario == $s and .lane == $l and ((.personaSet // "") == ""))] as $m
  | if ($m | length) != 1 then error("the discovery descriptor states no persona for \($s) on lane \($l)") else $m[0] end
  | . as $p
  | if ($p.memberId // "") == "" then error("the descriptor persona for \($s) names no member") else . end
  | ($keys + {member: $p.memberId})
  + (if $p.order == null then {} else
      (if ($p.order.claimType == null or $p.order.priority == null or $p.order.serviceCategory == null)
        then error("the descriptor persona \($p.memberId) states no claim type, priority or service category")
        else {order: {system: $p.order.system, code: $p.order.code},
              claimFacts: ({claimType: $p.order.claimType, priority: $p.order.priority,
                            serviceCategory: $p.order.serviceCategory, placeOfService: $p.order.placeOfService,
                            requestType: $p.order.requestType} | with_entries(select(.value != null)))}
          + (if $amend == 1 then
               (if (($p.amendment.certificationType.code // "") == "")
                 then error("the descriptor persona \($p.memberId) states no certificationType for its amendment")
                 else {certificationType: $p.amendment.certificationType} end)
             else {} end)
        end)
    end)
' "$desc"
