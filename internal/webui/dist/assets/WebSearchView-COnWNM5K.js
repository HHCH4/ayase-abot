import{f as V,S as se,E as Q,I as re}from"./Space-CEfSybN_.js";import{u as $e,C as M,B as U,T as G}from"./use-message-D8JEALQ8.js";import{A as le}from"./Alert-Ds5Gkq6O.js";import{C as ie}from"./CheckboxGroup-D9LSQZs7.js";import{S as xe}from"./Select-Cixfz8M-.js";import{M as ne}from"./Modal-CEfCVtf-.js";import{F as Ce,a as F}from"./FormItem-OcrDv7ED.js";import{I as Se}from"./InputNumber-DGQNRfug.js";import{d as Z,g as a,c as u,a as e,x as D,y as k,O as N,F as T,e as I,n as R,V as pe,W as ge,X as fe,Y as he,Z as Pe,T as ye,L as ee,s as C,M as O,B as ve,$ as ae,A as ze,D as Be,a0 as Ie,U as ue,o as Ne,b as n,w as d,u as i,m as X,r as L,a1 as Re,a2 as De,_ as oe,h as S,t as s,l as H,k as de,a3 as We,a4 as Ae,a5 as Te}from"./index-rtMR-bc1.js";import"./Add-Cm7kZUin.js";const qe=["id"],Le=["stop-color"],Ke=["stop-color"],Me=["viewBox"],Oe=["d","stroke-width"],je=["d","stroke-width"],Ue={success:(a(),I(he)),error:(a(),I(fe)),warning:(a(),I(ge)),info:(a(),I(pe))};var Ee=Z({name:"ProgressCircle",props:{clsPrefix:{type:String,required:!0},status:{type:String,required:!0},strokeWidth:{type:Number,required:!0},fillColor:[String,Object],railColor:String,railStyle:[String,Object],percentage:{type:Number,default:0},offsetDegree:{type:Number,default:0},showIndicator:{type:Boolean,required:!0},indicatorTextColor:String,unit:String,viewBoxWidth:{type:Number,required:!0},gapDegree:{type:Number,required:!0},gapOffsetDegree:{type:Number,default:0}},setup(l,{slots:v}){const x=R(()=>{const p="gradient",{fillColor:f}=l;return typeof f=="object"?`${p}-${Pe(JSON.stringify(f))}`:p});function c(p,f,y,w){const{gapDegree:P,viewBoxWidth:z,strokeWidth:$}=l,b=50,g=0,m=b,h=0,q=100,W=50+$/2,B=`M ${W},${W} m ${g},${m}
      a ${b},${b} 0 1 1 ${h},-100
      a ${b},${b} 0 1 1 0,${q}`,A=Math.PI*2*b;return{pathString:B,pathStyle:{stroke:w==="rail"?y:typeof l.fillColor=="object"?`url(#${x.value})`:y,strokeDasharray:`${Math.min(p,100)/100*(A-P)}px ${z*8}px`,strokeDashoffset:`-${P/2}px`,transformOrigin:f?"center":void 0,transform:f?`rotate(${f}deg)`:void 0}}}const _=()=>{const p=typeof l.fillColor=="object",f=p?l.fillColor.stops[0]:"",y=p?l.fillColor.stops[1]:"";return p&&(a(),u("defs",null,[e("linearGradient",{id:x.value,x1:"0%",y1:"100%",x2:"100%",y2:"0%"},[e("stop",{offset:"0%","stop-color":f},null,8,Le),e("stop",{offset:"100%","stop-color":y},null,8,Ke)],8,qe)]))};return()=>{const{fillColor:p,railColor:f,strokeWidth:y,offsetDegree:w,status:P,percentage:z,showIndicator:$,indicatorTextColor:b,unit:g,gapOffsetDegree:m,clsPrefix:h}=l,{pathString:q,pathStyle:W}=c(100,0,f,"rail"),{pathString:B,pathStyle:A}=c(z,w,p,"fill"),K=100+y;return a(),u("div",{class:k(`${h}-progress-content`),role:"none"},[e("div",{class:k(`${h}-progress-graph`),"aria-hidden":!0},[e("div",{class:k(`${h}-progress-graph-circle`),style:D({transform:m?`rotate(${m}deg)`:void 0})},[(a(),u("svg",{viewBox:`0 0 ${K} ${K}`},[N(()=>_()),e("g",null,[e("path",{class:k(`${h}-progress-graph-circle-rail`),d:q,"stroke-width":y,"stroke-linecap":"round",fill:"none",style:D(W)},null,14,Oe)]),e("g",null,[e("path",{class:k([`${h}-progress-graph-circle-fill`,z===0&&`${h}-progress-graph-circle-fill--empty`]),d:B,"stroke-width":y,"stroke-linecap":"round",fill:"none",style:D(A)},null,14,je)])],8,Me))],6)],2),$?(a(),u("div",{key:0},[v.default?(a(),u("div",{key:0,class:k(`${h}-progress-custom-content`),role:"none"},[N(()=>v.default())],2)):(a(),u(T,{key:1},[P!=="default"?(a(),u("div",{key:0,class:k(`${h}-progress-icon`),"aria-hidden":!0},[(a(),I(ye,{clsPrefix:h},{default:()=>Ue[P]},1032,["clsPrefix"]))],2)):(a(),u("div",{key:1,class:k(`${h}-progress-text`),style:D({color:b}),role:"none"},[e("span",{class:k(`${h}-progress-text__percentage`)},[N(()=>z)],2),e("span",{class:k(`${h}-progress-text__unit`)},[N(()=>g)],2)],6))],64))])):N(()=>null)],2)}}});const Ve={success:(a(),I(he)),error:(a(),I(fe)),warning:(a(),I(ge)),info:(a(),I(pe))};var Ge=Z({name:"ProgressLine",props:{clsPrefix:{type:String,required:!0},percentage:{type:Number,default:0},railColor:String,railStyle:[String,Object],fillColor:[String,Object],status:{type:String,required:!0},indicatorPlacement:{type:String,required:!0},indicatorTextColor:String,unit:{type:String,default:"%"},processing:{type:Boolean,required:!0},showIndicator:{type:Boolean,required:!0},height:[String,Number],railBorderRadius:[String,Number],fillBorderRadius:[String,Number]},setup(l,{slots:v}){const x=R(()=>V(l.height)),c=R(()=>typeof l.fillColor=="object"?`linear-gradient(to right, ${l.fillColor?.stops[0]} , ${l.fillColor?.stops[1]})`:l.fillColor),_=R(()=>l.railBorderRadius!==void 0?V(l.railBorderRadius):l.height!==void 0?V(l.height,{c:.5}):""),p=R(()=>l.fillBorderRadius!==void 0?V(l.fillBorderRadius):l.railBorderRadius!==void 0?V(l.railBorderRadius):l.height!==void 0?V(l.height,{c:.5}):"");return()=>{const{indicatorPlacement:f,railColor:y,railStyle:w,percentage:P,unit:z,indicatorTextColor:$,status:b,showIndicator:g,processing:m,clsPrefix:h}=l;return a(),u("div",{class:k(`${h}-progress-content`),role:"none"},[e("div",{class:k(`${h}-progress-graph`),"aria-hidden":!0},[e("div",{class:k([`${h}-progress-graph-line`,{[`${h}-progress-graph-line--indicator-${f}`]:!0}])},[e("div",{class:k(`${h}-progress-graph-line-rail`),style:D([{backgroundColor:y,height:x.value,borderRadius:_.value},w])},[e("div",{class:k([`${h}-progress-graph-line-fill`,m&&`${h}-progress-graph-line-fill--processing`]),style:D({maxWidth:`${l.percentage}%`,background:c.value,height:x.value,lineHeight:x.value,borderRadius:p.value})},[f==="inside"?(a(),u("div",{key:0,class:k(`${h}-progress-graph-line-indicator`),style:D({color:$})},[v.default?(a(),u(T,{key:0},[N(()=>v.default())],64)):(a(),u(T,{key:1},[N(()=>`${P}${z}`)],64))],6)):N(()=>null)],6)],6)],2)],2),g&&f==="outside"?(a(),u("div",{key:0},[v.default?(a(),u("div",{key:0,class:k(`${h}-progress-custom-content`),style:D({color:$}),role:"none"},[N(()=>v.default())],6)):(a(),u(T,{key:1},[b==="default"?(a(),u("div",{key:0,role:"none",class:k(`${h}-progress-icon ${h}-progress-icon--as-text`),style:D({color:$})},[N(()=>P),N(()=>z)],6)):(a(),u("div",{key:1,class:k(`${h}-progress-icon`),"aria-hidden":!0},[(a(),I(ye,{clsPrefix:h},{default:()=>Ve[b]},1032,["clsPrefix"]))],2))],64))])):N(()=>null)],2)}}});const Fe=["id"],Xe=["stop-color"],Ye=["stop-color"],He=["d","stroke-width"],Ze=["d","stroke-width"],Je=["viewBox"];function ce(l,v,x=100){return`m ${x/2} ${x/2-l} a ${l} ${l} 0 1 1 0 ${2*l} a ${l} ${l} 0 1 1 0 -${2*l}`}var Qe=Z({name:"ProgressMultipleCircle",props:{clsPrefix:{type:String,required:!0},viewBoxWidth:{type:Number,required:!0},percentage:{type:Array,default:[0]},strokeWidth:{type:Number,required:!0},circleGap:{type:Number,required:!0},showIndicator:{type:Boolean,required:!0},fillColor:{type:Array,default:()=>[]},railColor:{type:Array,default:()=>[]},railStyle:{type:Array,default:()=>[]}},setup(l,{slots:v}){const x=R(()=>l.percentage.map((_,p)=>`${Math.PI*_/100*(l.viewBoxWidth/2-l.strokeWidth/2*(1+2*p)-l.circleGap*p)*2}, ${l.viewBoxWidth*8}`)),c=(_,p)=>{const f=l.fillColor[p],y=typeof f=="object"?f.stops[0]:"",w=typeof f=="object"?f.stops[1]:"";return typeof l.fillColor[p]=="object"&&(a(),u("linearGradient",{id:`gradient-${p}`,x1:"100%",y1:"0%",x2:"0%",y2:"100%"},[e("stop",{offset:"0%","stop-color":y},null,8,Xe),e("stop",{offset:"100%","stop-color":w},null,8,Ye)],8,Fe))};return()=>{const{viewBoxWidth:_,strokeWidth:p,circleGap:f,showIndicator:y,fillColor:w,railColor:P,railStyle:z,percentage:$,clsPrefix:b}=l;return a(),u("div",{class:k(`${b}-progress-content`),role:"none"},[e("div",{class:k(`${b}-progress-graph`),"aria-hidden":!0},[e("div",{class:k(`${b}-progress-graph-circle`)},[(a(),u("svg",{viewBox:`0 0 ${_} ${_}`},[e("defs",null,[N(()=>$.map((g,m)=>c(g,m)))]),N(()=>$.map((g,m)=>(a(),u("g",{key:m},[e("path",{class:k(`${b}-progress-graph-circle-rail`),d:ce(_/2-p/2*(1+2*m)-f*m,p,_),"stroke-width":p,"stroke-linecap":"round",fill:"none",style:D([{strokeDashoffset:0,stroke:P[m]},z[m]])},null,14,He),e("path",{class:k([`${b}-progress-graph-circle-fill`,g===0&&`${b}-progress-graph-circle-fill--empty`]),d:ce(_/2-p/2*(1+2*m)-f*m,p,_),"stroke-width":p,"stroke-linecap":"round",fill:"none",style:D({strokeDasharray:x.value[m],strokeDashoffset:0,stroke:typeof w[m]=="object"?`url(#gradient-${m})`:w[m]})},null,14,Ze)]))))],8,Je))],2)],2),y&&v.default?(a(),u("div",{key:0},[e("div",{class:k(`${b}-progress-text`)},[N(()=>v.default())],2)])):N(()=>null)],2)}}}),et=ee([C("progress",{display:"inline-block"},[C("progress-icon",`
 color: var(--n-icon-color);
 transition: color .3s var(--n-bezier);
 `),O("line",`
 width: 100%;
 display: block;
 `,[C("progress-content",`
 display: flex;
 align-items: center;
 `,[C("progress-graph",{flex:1})]),C("progress-custom-content",{marginLeft:"14px"}),C("progress-icon",`
 width: 30px;
 padding-left: 14px;
 height: var(--n-icon-size-line);
 line-height: var(--n-icon-size-line);
 font-size: var(--n-icon-size-line);
 `,[O("as-text",`
 color: var(--n-text-color-line-outer);
 text-align: center;
 width: 40px;
 font-size: var(--n-font-size);
 padding-left: 4px;
 transition: color .3s var(--n-bezier);
 `)])]),O("circle, dashboard",{width:"120px"},[C("progress-custom-content",`
 position: absolute;
 left: 50%;
 top: 50%;
 transform: translateX(-50%) translateY(-50%);
 display: flex;
 align-items: center;
 justify-content: center;
 `),C("progress-text",`
 position: absolute;
 left: 50%;
 top: 50%;
 transform: translateX(-50%) translateY(-50%);
 display: flex;
 align-items: center;
 color: inherit;
 font-size: var(--n-font-size-circle);
 color: var(--n-text-color-circle);
 font-weight: var(--n-font-weight-circle);
 transition: color .3s var(--n-bezier);
 white-space: nowrap;
 `),C("progress-icon",`
 position: absolute;
 left: 50%;
 top: 50%;
 transform: translateX(-50%) translateY(-50%);
 display: flex;
 align-items: center;
 color: var(--n-icon-color);
 font-size: var(--n-icon-size-circle);
 `)]),O("multiple-circle",`
 width: 200px;
 color: inherit;
 `,[C("progress-text",`
 font-weight: var(--n-font-weight-circle);
 color: var(--n-text-color-circle);
 position: absolute;
 left: 50%;
 top: 50%;
 transform: translateX(-50%) translateY(-50%);
 display: flex;
 align-items: center;
 justify-content: center;
 transition: color .3s var(--n-bezier);
 `)]),C("progress-content",{position:"relative"}),C("progress-graph",{position:"relative"},[C("progress-graph-circle",[ee("svg",{verticalAlign:"bottom"}),C("progress-graph-circle-fill",`
 stroke: var(--n-fill-color);
 transition:
 opacity .3s var(--n-bezier),
 stroke .3s var(--n-bezier),
 stroke-dasharray .3s var(--n-bezier);
 `,[O("empty",{opacity:0})]),C("progress-graph-circle-rail",`
 transition: stroke .3s var(--n-bezier);
 overflow: hidden;
 stroke: var(--n-rail-color);
 `)]),C("progress-graph-line",[O("indicator-inside",[C("progress-graph-line-rail",`
 height: 16px;
 line-height: 16px;
 border-radius: 10px;
 `,[C("progress-graph-line-fill",`
 height: inherit;
 border-radius: 10px;
 `),C("progress-graph-line-indicator",`
 background: #0000;
 white-space: nowrap;
 text-align: right;
 margin-left: 14px;
 margin-right: 14px;
 height: inherit;
 font-size: 12px;
 color: var(--n-text-color-line-inner);
 transition: color .3s var(--n-bezier);
 `)])]),O("indicator-inside-label",`
 height: 16px;
 display: flex;
 align-items: center;
 `,[C("progress-graph-line-rail",`
 flex: 1;
 transition: background-color .3s var(--n-bezier);
 `),C("progress-graph-line-indicator",`
 background: var(--n-fill-color);
 font-size: 12px;
 transform: translateZ(0);
 display: flex;
 vertical-align: middle;
 height: 16px;
 line-height: 16px;
 padding: 0 10px;
 border-radius: 10px;
 position: absolute;
 white-space: nowrap;
 color: var(--n-text-color-line-inner);
 transition:
 right .2s var(--n-bezier),
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier);
 `)]),C("progress-graph-line-rail",`
 position: relative;
 overflow: hidden;
 height: var(--n-rail-height);
 border-radius: 5px;
 background-color: var(--n-rail-color);
 transition: background-color .3s var(--n-bezier);
 `,[C("progress-graph-line-fill",`
 background: var(--n-fill-color);
 position: relative;
 border-radius: 5px;
 height: inherit;
 width: 100%;
 max-width: 0%;
 transition:
 background-color .3s var(--n-bezier),
 max-width .2s var(--n-bezier);
 `,[O("processing",[ee("&::after",`
 content: "";
 background-image: var(--n-line-bg-processing);
 animation: progress-processing-animation 2s var(--n-bezier) infinite;
 `)])])])])])]),ee("@keyframes progress-processing-animation",`
 0% {
 position: absolute;
 left: 0;
 top: 0;
 bottom: 0;
 right: 100%;
 opacity: 1;
 }
 66% {
 position: absolute;
 left: 0;
 top: 0;
 bottom: 0;
 right: 0;
 opacity: 0;
 }
 100% {
 position: absolute;
 left: 0;
 top: 0;
 bottom: 0;
 right: 0;
 opacity: 0;
 }
 `)]);const tt=["aria-valuenow","role"],rt={...ve.props,processing:Boolean,type:{type:String,default:"line"},gapDegree:Number,gapOffsetDegree:Number,status:{type:String,default:"default"},railColor:[String,Array],railStyle:[String,Array],color:[String,Array,Object],viewBoxWidth:{type:Number,default:100},strokeWidth:{type:Number,default:7},percentage:[Number,Array],unit:{type:String,default:"%"},showIndicator:{type:Boolean,default:!0},indicatorPosition:{type:String,default:"outside"},indicatorPlacement:{type:String,default:"outside"},indicatorTextColor:String,circleGap:{type:Number,default:1},height:Number,borderRadius:[String,Number],fillBorderRadius:[String,Number],offsetDegree:Number};var lt=Z({name:"Progress",props:rt,setup(l){const v=R(()=>l.indicatorPlacement||l.indicatorPosition),x=R(()=>{if(l.gapDegree||l.gapDegree===0)return l.gapDegree;if(l.type==="dashboard")return 75}),{mergedClsPrefixRef:c,inlineThemeDisabled:_}=ze(l),p=ve("Progress","-progress",et,Ie,l,c),f=R(()=>{const{status:w}=l,{common:{cubicBezierEaseInOut:P},self:{fontSize:z,fontSizeCircle:$,railColor:b,railHeight:g,iconSizeCircle:m,iconSizeLine:h,textColorCircle:q,textColorLineInner:W,textColorLineOuter:B,lineBgProcessing:A,fontWeightCircle:K,[ue("iconColor",w)]:Y,[ue("fillColor",w)]:j}}=p.value;return{"--n-bezier":P,"--n-fill-color":j,"--n-font-size":z,"--n-font-size-circle":$,"--n-font-weight-circle":K,"--n-icon-color":Y,"--n-icon-size-circle":m,"--n-icon-size-line":h,"--n-line-bg-processing":A,"--n-rail-color":b,"--n-rail-height":g,"--n-text-color-circle":q,"--n-text-color-line-inner":W,"--n-text-color-line-outer":B}}),y=_?Be("progress",R(()=>l.status[0]),f,l):void 0;return{mergedClsPrefix:c,mergedIndicatorPlacement:v,gapDeg:x,cssVars:_?void 0:f,themeClass:y?.themeClass,onRender:y?.onRender}},render(){const{type:l,cssVars:v,indicatorTextColor:x,showIndicator:c,status:_,railColor:p,railStyle:f,color:y,percentage:w,viewBoxWidth:P,strokeWidth:z,mergedIndicatorPlacement:$,unit:b,borderRadius:g,fillBorderRadius:m,height:h,processing:q,circleGap:W,mergedClsPrefix:B,gapDeg:A,gapOffsetDegree:K,themeClass:Y,$slots:j,onRender:J}=this;return J?.(),a(),u("div",{class:k([Y,`${B}-progress`,`${B}-progress--${l}`,`${B}-progress--${_}`]),style:D(v),"aria-valuemax":100,"aria-valuemin":0,"aria-valuenow":w,role:l==="circle"||l==="line"||l==="dashboard"?"progressbar":"none"},[l==="circle"||l==="dashboard"?(a(),I(Ee,{key:0,clsPrefix:B,status:_,showIndicator:c,indicatorTextColor:x,railColor:p,fillColor:y,railStyle:f,offsetDegree:this.offsetDegree,percentage:w,viewBoxWidth:P,strokeWidth:z,gapDegree:A===void 0?l==="dashboard"?75:0:A,gapOffsetDegree:K,unit:b},ae(j),1032,["clsPrefix","status","showIndicator","indicatorTextColor","railColor","fillColor","railStyle","offsetDegree","percentage","viewBoxWidth","strokeWidth","gapDegree","gapOffsetDegree","unit"])):(a(),u(T,{key:1},[l==="line"?(a(),I(Ge,{key:0,clsPrefix:B,status:_,showIndicator:c,indicatorTextColor:x,railColor:p,fillColor:y,railStyle:f,percentage:w,processing:q,indicatorPlacement:$,unit:b,fillBorderRadius:m,railBorderRadius:g,height:h},ae(j),1032,["clsPrefix","status","showIndicator","indicatorTextColor","railColor","fillColor","railStyle","percentage","processing","indicatorPlacement","unit","fillBorderRadius","railBorderRadius","height"])):(a(),u(T,{key:1},[l==="multiple-circle"?(a(),I(Qe,{key:0,clsPrefix:B,strokeWidth:z,railColor:p,fillColor:y,railStyle:f,viewBoxWidth:P,percentage:w,showIndicator:c,circleGap:W},ae(j),1032,["clsPrefix","strokeWidth","railColor","fillColor","railStyle","viewBoxWidth","percentage","showIndicator","circleGap"])):N(()=>null)],64))],64))],14,tt)}});const at={class:"page-view"},ot={class:"page-intro"},st={key:1,class:"stats-grid web-search-stats"},it={class:"section-heading-row"},nt={key:1,class:"web-search-service-list"},ut={class:"web-search-service-main"},dt={class:"web-search-service-title"},ct={class:"section-heading-row"},pt={key:0,class:"data-table-wrap web-search-table-wrap"},gt={class:"data-table web-search-usage-table"},ft={key:0,class:"data-table-wrap web-search-table-wrap"},ht={class:"data-table web-search-usage-table"},yt={key:0,class:"data-table-wrap web-search-table-wrap"},vt={class:"data-table web-search-table"},bt={class:"web-search-query"},mt={key:0},kt={class:"form-grid-2"},_t={class:"modal-footer"},wt={key:0,class:"web-search-test-content"},$t=["href"],xt={key:0,class:"muted"},At=Z({__name:"WebSearchView",setup(l){const v=$e(),x=L([]),c=L(null),_=L(!1),p=L(!1),f=L(!1),y=L(""),w=L(!1),P=L(!1),z=de({}),$=L(null),b=[{label:"Tavily",value:"tavily"},{label:"LangSearch",value:"langsearch"}],g=de(W()),m=R(()=>[...x.value].sort((o,t)=>o.priority-t.priority||o.name.localeCompare(t.name))),h=R(()=>{const o=c.value?.daily_call_limit||0;return o?Math.max(0,Math.min(100,Math.round((c.value?.daily_calls||0)*100/o))):0}),q=R(()=>new Set(x.value.map(o=>`${o.provider}:${o.account_group||o.id}`)).size);function W(){return{name:"",provider:"tavily",account_group:"",api_key:"",enabled:!0,priority:100}}async function B(){_.value=!0;try{const[o,t]=await Promise.all([Re(),De()]);x.value=o,c.value=t}catch(o){v.error(o instanceof Error?o.message:"读取网页搜索配置失败")}finally{_.value=!1}}function A(o){y.value=o?.id||"",w.value=!1,Object.assign(g,o?{name:o.name,provider:o.provider==="langsearch"?"langsearch":"tavily",account_group:o.account_group||"",api_key:"",enabled:o.enabled,priority:o.priority}:W()),f.value=!0}async function K(){const o=g.name.trim();if(!o){v.warning("请填写服务名称");return}if(!y.value&&!g.api_key.trim()){v.warning("新服务必须填写 API Key");return}if(w.value&&g.enabled){v.warning("清除 API Key 前请先停用该服务");return}p.value=!0;try{const t={id:y.value||void 0,name:o,provider:g.provider,account_group:g.account_group.trim(),enabled:g.enabled,priority:g.priority||0};g.api_key.trim()?t.api_key=g.api_key.trim():w.value&&(t.api_key=""),await We(t),f.value=!1,v.success("网页搜索服务已保存"),await B()}catch(t){v.error(t instanceof Error?t.message:"保存网页搜索服务失败")}finally{p.value=!1}}async function Y(o){if(window.confirm(`删除“${o.name}”？历史用量会保留；配置档正在引用时需要先移除引用。`))try{await Ae(o.id),v.success("网页搜索服务已删除"),await B()}catch(t){v.error(t instanceof Error?t.message:"删除网页搜索服务失败")}}async function j(o){z[o.id]=!0;try{$.value=await Te(o.id),P.value=!0;const t=te(o.provider,$.value.usage);v.success(`连接成功，返回 ${$.value.results.length} 条结果；本次用量：${t}`),await B()}catch(t){v.error(t instanceof Error?t.message:"搜索服务测试失败"),await B()}finally{delete z[o.id]}}function J(o){return c.value?.by_service.find(t=>t.service_id===o)}function te(o,t){return t?.known?o==="tavily"?`${t.credits} credits`:`${t.input_tokens} 输入 / ${t.output_tokens} 输出 tokens`:"服务商未返回可计量用量"}function be(o){const t=J(o.id);return t?o.provider==="tavily"?`${t.credits} credits`:`${t.input_tokens+t.output_tokens} tokens`:"暂无记录"}function me(o){return te(o.provider,o.usage)}function ke(o){return o.account_group||"独立额度组"}function _e(o){if(!o)return"—";const t=new Date(o);return Number.isNaN(t.getTime())?o:t.toLocaleString()}function we(o){return o==="success"?"success":o==="pending"?"warning":o==="failed"?"error":"default"}return Ne(()=>{B()}),(o,t)=>(a(),u("div",at,[e("div",ot,[t[13]||(t[13]=e("div",null,[e("p",{class:"eyebrow"},"WEB SEARCH"),e("h2",null,"网页搜索"),e("p",null,"为内置 Agent 配置 Tavily、LangSearch 搜索服务。密钥只保存在服务端；按服务优先级回退，并记录每次请求与服务商返回的用量。")],-1)),n(i(se),null,{default:d(()=>[n(i(U),{secondary:"",loading:_.value,onClick:B},{default:d(()=>[n(oe,{name:"refresh",size:14}),t[11]||(t[11]=S("刷新用量",-1))]),_:1},8,["loading"]),n(i(U),{type:"primary",onClick:t[0]||(t[0]=r=>A())},{default:d(()=>[n(oe,{name:"plus",size:14}),t[12]||(t[12]=S("添加服务",-1))]),_:1})]),_:1})]),c.value?.alert?(a(),I(i(le),{key:0,type:"warning","show-icon":!0,class:"web-search-alert"},{default:d(()=>[S(" 已达到每日搜索上限的 "+s(c.value.alert_percent)+"%（"+s(c.value.daily_calls)+" / "+s(c.value.daily_call_limit)+" 次）。Abot 会在硬上限处停止向搜索服务发请求。 ",1)]),_:1})):X("",!0),c.value?(a(),u("div",st,[n(i(M),{class:"stat-card",bordered:!1},{default:d(()=>[t[14]||(t[14]=e("span",null,"今日请求",-1)),e("strong",null,s(c.value.daily_calls)+" / "+s(c.value.daily_call_limit),1),t[15]||(t[15]=e("span",null,"按 UTC 日期统计，每次服务尝试计一次",-1))]),_:1}),n(i(M),{class:"stat-card",bordered:!1},{default:d(()=>[t[16]||(t[16]=e("span",null,"本月已配置服务",-1)),e("strong",null,s(x.value.length),1),e("span",null,s(q.value)+" 个服务商额度组",1)]),_:1}),n(i(M),{class:"stat-card",bordered:!1},{default:d(()=>[t[17]||(t[17]=e("span",null,"本月成功 / 失败",-1)),e("strong",null,s(c.value.by_service.reduce((r,E)=>r+E.successes,0))+" / "+s(c.value.by_service.reduce((r,E)=>r+E.failures,0)),1),t[18]||(t[18]=e("span",null,"包括优先级回退尝试",-1))]),_:1}),n(i(M),{class:"stat-card web-search-progress-card",bordered:!1},{default:d(()=>[t[19]||(t[19]=e("span",null,"每日预算使用",-1)),n(i(lt),{type:"line",percentage:h.value,status:c.value.alert?"warning":"success","show-indicator":!0},null,8,["percentage","status"])]),_:1})])):X("",!0),n(i(M),{class:"web-search-card",bordered:!1},{default:d(()=>[e("div",it,[t[20]||(t[20]=e("div",null,[e("h3",null,"搜索服务"),e("p",null,"相同服务商且额度组相同的 Key 会作为同一账号处理；收到限额错误后暂时跳过该组的其他 Key，再尝试下一组。")],-1)),n(i(G),{size:"small",bordered:!1},{default:d(()=>[S(s(x.value.length)+" 个实例",1)]),_:1})]),x.value.length?(a(),u("div",nt,[(a(!0),u(T,null,H(m.value,r=>(a(),u("article",{key:r.id,class:"web-search-service-row"},[e("div",ut,[e("div",dt,[e("strong",null,s(r.name),1),n(i(G),{size:"small",bordered:!1,type:r.provider==="tavily"?"info":"success"},{default:d(()=>[S(s(r.provider),1)]),_:2},1032,["type"]),n(i(G),{size:"small",bordered:!1,type:r.enabled?"success":"default"},{default:d(()=>[S(s(r.enabled?"已启用":"已停用"),1)]),_:2},1032,["type"]),n(i(G),{size:"small",bordered:!1,type:r.api_key_configured?"default":"warning"},{default:d(()=>[S(s(r.api_key_configured?"Key 已保存":"缺少 Key"),1)]),_:2},1032,["type"])]),e("code",null,s(r.id),1),e("span",null,"额度组："+s(ke(r))+" · 服务优先级 "+s(r.priority),1),e("span",null,"本月 "+s(J(r.id)?.calls||0)+" 次请求 · "+s(be(r)),1)]),n(i(se),{class:"web-search-service-actions",size:6},{default:d(()=>[n(i(U),{size:"small",secondary:"",disabled:!r.enabled||!r.api_key_configured,loading:z[r.id]===!0,onClick:E=>j(r)},{default:d(()=>[...t[21]||(t[21]=[S("测试",-1)])]),_:1},8,["disabled","loading","onClick"]),n(i(U),{size:"small",secondary:"",onClick:E=>A(r)},{default:d(()=>[...t[22]||(t[22]=[S("编辑",-1)])]),_:1},8,["onClick"]),n(i(U),{size:"small",tertiary:"",type:"error",onClick:E=>Y(r)},{default:d(()=>[n(oe,{name:"trash",size:14}),t[23]||(t[23]=S("删除",-1))]),_:1},8,["onClick"])]),_:2},1024)]))),128))])):(a(),I(i(Q),{key:0,description:"还没有搜索服务。先添加 Tavily 或 LangSearch API Key，再到配置文件中启用网页搜索。"}))]),_:1}),n(i(M),{class:"web-search-card",bordered:!1},{default:d(()=>[e("div",ct,[t[24]||(t[24]=e("div",null,[e("h3",null,"本月账号额度组汇总"),e("p",null,"相同服务商、相同额度组的多个 Key 合并统计。这里只计 Abot 发起的请求；同一账号经 MCP 或其他客户端的调用也会占上游额度，但不在 Abot 账本内。")],-1)),n(i(G),{size:"small",bordered:!1},{default:d(()=>[S(s(c.value?.period_start?new Date(c.value.period_start).toLocaleDateString():"")+" 起",1)]),_:1})]),c.value?.by_account.length?(a(),u("div",pt,[e("table",gt,[t[25]||(t[25]=e("thead",null,[e("tr",null,[e("th",null,"服务商 / 额度组"),e("th",null,"请求"),e("th",null,"成功 / 失败"),e("th",null,"Tavily credits"),e("th",null,"LangSearch tokens"),e("th",null,"用量未知")])],-1)),e("tbody",null,[(a(!0),u(T,null,H(c.value.by_account,r=>(a(),u("tr",{key:`${r.provider}:${r.account_group}`},[e("td",null,[e("strong",null,s(r.provider),1),e("code",null,s(r.account_group),1)]),e("td",null,s(r.calls),1),e("td",null,s(r.successes)+" / "+s(r.failures),1),e("td",null,s(r.credits),1),e("td",null,s(r.input_tokens)+" / "+s(r.output_tokens),1),e("td",null,s(r.unknown_usage),1)]))),128))])])])):(a(),I(i(Q),{key:1,description:"尚无已配置账号额度组"}))]),_:1}),n(i(M),{class:"web-search-card",bordered:!1},{default:d(()=>[t[27]||(t[27]=e("div",{class:"section-heading-row"},[e("div",null,[e("h3",null,"按 API Key 查看本月用量"),e("p",null,"Tavily 记录服务返回的 credits；LangSearch 记录返回的输入、输出 tokens。服务商不返回用量时会标记为未知，不作估算。")])],-1)),c.value?.by_service.length?(a(),u("div",ft,[e("table",ht,[t[26]||(t[26]=e("thead",null,[e("tr",null,[e("th",null,"服务 / 额度组"),e("th",null,"请求"),e("th",null,"成功 / 失败"),e("th",null,"Tavily credits"),e("th",null,"LangSearch tokens"),e("th",null,"用量未知")])],-1)),e("tbody",null,[(a(!0),u(T,null,H(c.value.by_service,r=>(a(),u("tr",{key:r.service_id},[e("td",null,[e("strong",null,s(r.service_name),1),e("code",null,s(r.provider)+" · "+s(r.account_group||"独立额度组"),1)]),e("td",null,s(r.calls),1),e("td",null,s(r.successes)+" / "+s(r.failures),1),e("td",null,s(r.credits),1),e("td",null,s(r.input_tokens)+" / "+s(r.output_tokens),1),e("td",null,s(r.unknown_usage),1)]))),128))])])])):(a(),I(i(Q),{key:1,description:"本月尚无网页搜索请求"}))]),_:1}),n(i(M),{class:"web-search-card",bordered:!1},{default:d(()=>[t[29]||(t[29]=e("div",{class:"section-heading-row"},[e("div",null,[e("h3",null,"最近请求"),e("p",null,"展示近 90 天记录；删除会话会清空查询文本、任务 ID 和会话关联，但匿名用量仍计入预算。")])],-1)),c.value?.recent.length?(a(),u("div",yt,[e("table",vt,[t[28]||(t[28]=e("thead",null,[e("tr",null,[e("th",null,"时间"),e("th",null,"查询"),e("th",null,"服务"),e("th",null,"状态"),e("th",null,"结果数"),e("th",null,"服务商用量")])],-1)),e("tbody",null,[(a(!0),u(T,null,H(c.value.recent,r=>(a(),u("tr",{key:r.id},[e("td",null,s(_e(r.created_at)),1),e("td",bt,s(r.query),1),e("td",null,[e("strong",null,s(r.service_name),1),e("code",null,s(r.provider)+" · "+s(r.account_group||"独立额度组"),1)]),e("td",null,[n(i(G),{size:"small",bordered:!1,type:we(r.status)},{default:d(()=>[S(s(r.status==="success"?"成功":r.status==="pending"?"处理中":"失败"),1)]),_:2},1032,["type"]),r.http_status?(a(),u("code",mt,"HTTP "+s(r.http_status),1)):X("",!0)]),e("td",null,s(r.result_count),1),e("td",null,s(me(r)),1)]))),128))])])])):(a(),I(i(Q),{key:1,description:"暂无网页搜索记录"}))]),_:1}),n(i(ne),{show:f.value,"onUpdate:show":t[9]||(t[9]=r=>f.value=r),preset:"card",title:y.value?"编辑搜索服务":"添加搜索服务",class:"web-search-editor-modal","mask-closable":!1},{footer:d(()=>[e("div",_t,[n(i(U),{onClick:t[8]||(t[8]=r=>f.value=!1)},{default:d(()=>[...t[34]||(t[34]=[S("取消",-1)])]),_:1}),n(i(U),{type:"primary",loading:p.value,onClick:K},{default:d(()=>[...t[35]||(t[35]=[S("保存服务",-1)])]),_:1},8,["loading"])])]),default:d(()=>[n(i(Ce),{"label-placement":"top"},{default:d(()=>[n(i(F),{label:"服务名称",required:""},{default:d(()=>[n(i(re),{value:g.name,"onUpdate:value":t[1]||(t[1]=r=>g.name=r),placeholder:"例如：Tavily 主账号"},null,8,["value"])]),_:1}),n(i(F),{label:"服务商",required:""},{default:d(()=>[n(i(xe),{value:g.provider,"onUpdate:value":t[2]||(t[2]=r=>g.provider=r),options:b},null,8,["value"])]),_:1}),n(i(F),{label:"API Key",required:""},{feedback:d(()=>[S(s(y.value?"Key 不会回传到浏览器；留空保留已保存的 Key。":"Key 只保存于 Abot 服务端，不会出现在配置档或 API 响应中。"),1)]),default:d(()=>[n(i(re),{value:g.api_key,"onUpdate:value":t[3]||(t[3]=r=>g.api_key=r),type:"password","show-password-on":"click",placeholder:y.value?"留空表示保留当前 Key":"请输入 API Key"},null,8,["value","placeholder"])]),_:1}),y.value?(a(),I(i(ie),{key:0,checked:w.value,"onUpdate:checked":t[4]||(t[4]=r=>w.value=r)},{default:d(()=>[...t[30]||(t[30]=[S("清除已保存的 API Key（需同时停用服务）",-1)])]),_:1},8,["checked"])):X("",!0),n(i(F),{label:"账号额度组"},{feedback:d(()=>[...t[31]||(t[31]=[S("同一个服务商的相同登录账号请填写同一组名；不同账号使用不同组名。留空时视为独立额度组。",-1)])]),default:d(()=>[n(i(re),{value:g.account_group,"onUpdate:value":t[5]||(t[5]=r=>g.account_group=r),placeholder:"例如 tavily-account-main"},null,8,["value"])]),_:1}),e("div",kt,[n(i(F),{label:"调用优先级"},{default:d(()=>[n(i(Se),{value:g.priority,"onUpdate:value":t[6]||(t[6]=r=>g.priority=r),min:0,max:1e4,step:1,"show-button":!1},null,8,["value"])]),_:1}),n(i(F),{label:"状态"},{default:d(()=>[n(i(ie),{checked:g.enabled,"onUpdate:checked":t[7]||(t[7]=r=>g.enabled=r)},{default:d(()=>[...t[32]||(t[32]=[S("启用",-1)])]),_:1},8,["checked"])]),_:1})])]),_:1}),n(i(le),{type:"info","show-icon":!1},{default:d(()=>[...t[33]||(t[33]=[S("发生限额或临时服务错误时，Abot 会按优先级尝试其他账号组；同一额度组中的其他 Key 会暂时跳过。",-1)])]),_:1})]),_:1},8,["show","title"]),n(i(ne),{show:P.value,"onUpdate:show":t[10]||(t[10]=r=>P.value=r),preset:"card",title:"网页搜索测试结果",class:"web-search-test-modal"},{default:d(()=>[$.value?(a(),u("div",wt,[n(i(le),{type:"success","show-icon":!1},{default:d(()=>[S(s($.value.provider)+" · 用量："+s(te($.value.provider,$.value.usage)),1)]),_:1}),(a(!0),u(T,null,H($.value.results,r=>(a(),u("article",{key:r.url,class:"web-search-result"},[e("strong",null,s(r.title||r.url),1),e("a",{href:r.url,target:"_blank",rel:"noreferrer"},s(r.url),9,$t),e("p",null,s(r.snippet),1)]))),128)),$.value.results.length?X("",!0):(a(),u("p",xt,"请求成功，但服务商没有返回搜索结果。"))])):X("",!0)]),_:1},8,["show"])]))}});export{At as default};
