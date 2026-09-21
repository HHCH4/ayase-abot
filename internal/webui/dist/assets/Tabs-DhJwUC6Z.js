import{Y as Wt,Z as pe,_ as Et,d as ee,$ as At,a0 as It,r as I,a1 as Ue,a as q,a2 as Ot,g as i,c as p,a3 as v,y as Q,z as R,a4 as Xe,a5 as jt,E as ve,F,e as E,I as Ge,a6 as Ht,a7 as Ft,n as ne,a8 as Mt,v as r,a9 as y,aa as l,ab as k,ac as Dt,x as Ye,A as Nt,ad as Vt,j as oe,o as Ut,ae as Xt,B as Gt,af as Ae,ag as Yt,C as ie,ah as G,ai as qt,aj as Kt,ak as Zt,al as Jt,am as Qt,G as Y}from"./index-D-X-jse3.js";import{B as ea,V as Ce,r as Ie,g as fe,b as ue}from"./use-message-C6NF5k8M.js";import{f as we,a as ta}from"./Space-CJ_sPJW9.js";import{A as aa}from"./Add-CiCUkgG5.js";import{c as ra,a as Oe,o as na,u as je}from"./Select-DEPwsd6R.js";var oa=/\s/;function ia(e){for(var n=e.length;n--&&oa.test(e.charAt(n)););return n}var sa=/^\s+/;function la(e){return e&&e.slice(0,ia(e)+1).replace(sa,"")}var He=NaN,da=/^[-+]0x[0-9a-f]+$/i,ca=/^0b[01]+$/i,ba=/^0o[0-7]+$/i,fa=parseInt;function Fe(e){if(typeof e=="number")return e;if(Wt(e))return He;if(pe(e)){var n=typeof e.valueOf=="function"?e.valueOf():e;e=pe(n)?n+"":n}if(typeof e!="string")return e===0?e:+e;e=la(e);var s=ca.test(e);return s||ba.test(e)?fa(e.slice(2),s?2:8):da.test(e)?He:+e}var Re=function(){return Et.Date.now()},ua="Expected a function",pa=Math.max,va=Math.min;function ha(e,n,s){var f,c,S,h,b,g,m=0,O=!1,B=!1,A=!0;if(typeof e!="function")throw new TypeError(ua);n=Fe(n)||0,pe(s)&&(O=!!s.leading,B="maxWait"in s,S=B?pa(Fe(s.maxWait)||0,n):S,A="trailing"in s?!!s.trailing:A);function j(u){var $=f,N=c;return f=c=void 0,m=u,h=e.apply(N,$),h}function L(u){return m=u,b=setTimeout(_,n),O?j(u):h}function z(u){var $=u-g,N=u-m,H=n-$;return B?va(H,S-N):H}function M(u){var $=u-g,N=u-m;return g===void 0||$>=n||$<0||B&&N>=S}function _(){var u=Re();if(M(u))return K(u);b=setTimeout(_,z(u))}function K(u){return b=void 0,A&&f?j(u):(f=c=void 0,h)}function J(){b!==void 0&&clearTimeout(b),m=0,f=g=c=b=void 0}function U(){return b===void 0?h:K(Re())}function D(){var u=Re(),$=M(u);if(f=arguments,c=this,g=u,$){if(b===void 0)return L(g);if(B)return clearTimeout(b),b=setTimeout(_,n),j(g)}return b===void 0&&(b=setTimeout(_,n)),h}return D.cancel=J,D.flush=U,D}var ga="Expected a function";function ma(e,n,s){var f=!0,c=!0;if(typeof e!="function")throw new TypeError(ga);return pe(s)&&(f="leading"in s?!!s.leading:f,c="trailing"in s?!!s.trailing:c),ha(e,n,{leading:f,maxWait:n,trailing:c})}const xa=Oe(".v-x-scroll",{overflow:"auto",scrollbarWidth:"none"},[Oe("&::-webkit-scrollbar",{width:0,height:0})]),ya=ee({name:"XScroll",props:{disabled:Boolean,onScroll:Function},setup(){const e=I(null);function n(c){!(c.currentTarget.offsetWidth<c.currentTarget.scrollWidth)||c.deltaY===0||(c.currentTarget.scrollLeft+=c.deltaY+c.deltaX,c.preventDefault())}const s=It();return xa.mount({id:"vueuc/x-scroll",head:!0,anchorMetaName:ra,ssr:s}),Object.assign({selfRef:e,handleWheel:n},{scrollTo(...c){var S;(S=e.value)===null||S===void 0||S.scrollTo(...c)}})},render(){return At("div",{ref:"selfRef",onScroll:this.onScroll,onWheel:this.disabled?void 0:this.handleWheel,class:"v-x-scroll"},this.$slots)}});var Ca=ee({name:"ChevronLeft",render(){return(()=>{const e=Ue("dfe229c2639b2082");return e[0]||(e[0]=q("svg",{viewBox:"0 0 16 16",fill:"none",xmlns:"http://www.w3.org/2000/svg"},[q("path",{d:"M10.3536 3.14645C10.5488 3.34171 10.5488 3.65829 10.3536 3.85355L6.20711 8L10.3536 12.1464C10.5488 12.3417 10.5488 12.6583 10.3536 12.8536C10.1583 13.0488 9.84171 13.0488 9.64645 12.8536L5.14645 8.35355C4.95118 8.15829 4.95118 7.84171 5.14645 7.64645L9.64645 3.14645C9.84171 2.95118 10.1583 2.95118 10.3536 3.14645Z",fill:"currentColor"})],-1))})()}}),wa=ee({name:"ChevronRight",render(){return(()=>{const e=Ue("6ab04425f4fcb756");return e[0]||(e[0]=q("svg",{viewBox:"0 0 16 16",fill:"none",xmlns:"http://www.w3.org/2000/svg"},[q("path",{d:"M5.64645 3.14645C5.45118 3.34171 5.45118 3.65829 5.64645 3.85355L9.79289 8L5.64645 12.1464C5.45118 12.3417 5.45118 12.6583 5.64645 12.8536C5.84171 13.0488 6.15829 13.0488 6.35355 12.8536L10.8536 8.35355C11.0488 8.15829 11.0488 7.84171 10.8536 7.64645L6.35355 3.14645C6.15829 2.95118 5.84171 2.95118 5.64645 3.14645Z",fill:"currentColor"})],-1))})()}});const $e=Ot("n-tabs"),qe={tab:[String,Number,Object,Function],name:{type:[String,Number],required:!0},disabled:Boolean,displayDirective:{type:String,default:"if"},closable:{type:Boolean,default:void 0},tabProps:Object,label:[String,Number,Object,Function]};var _a=ee({__TAB_PANE__:!0,name:"TabPane",alias:["TabPanel"],props:qe,slots:Object,setup(e){const n=Xe($e,null);return n||jt("tab-pane","`n-tab-pane` must be placed inside `n-tabs`."),{style:n.paneStyleRef,class:n.paneClassRef,mergedClsPrefix:n.mergedClsPrefixRef}},render(){return i(),p("div",{class:R([`${this.mergedClsPrefix}-tab-pane`,this.class]),style:Q(this.style)},[v(()=>this.$slots.default?.())],6)}});const Ra=["data-name","data-disabled"],Sa={internalLeftPadded:Boolean,internalAddable:Boolean,internalCreatedByPane:Boolean,...Mt(qe,["displayDirective"])};var ze=ee({__TAB__:!0,inheritAttrs:!1,name:"Tab",props:Sa,setup(e){const{mergedClsPrefixRef:n,valueRef:s,typeRef:f,closableRef:c,tabStyleRef:S,addTabStyleRef:h,tabClassRef:b,addTabClassRef:g,tabChangeIdRef:m,onBeforeLeaveRef:O,triggerRef:B,handleAdd:A,activateTab:j,handleClose:L}=Xe($e);return{trigger:B,mergedClosable:ne(()=>{if(e.internalAddable)return!1;const{closable:z}=e;return z===void 0?c.value:z}),style:S,addStyle:h,tabClass:b,addTabClass:g,clsPrefix:n,value:s,type:f,handleClose(z){z.stopPropagation(),!e.disabled&&L(e.name)},activateTab(){if(e.disabled)return;if(e.internalAddable){A();return}const{name:z}=e,M=++m.id;if(z!==s.value){const{value:_}=O;_?Promise.resolve(_(e.name,s.value)).then(K=>{K&&m.id===M&&j(z)}):j(z)}}}},render(){const{internalAddable:e,clsPrefix:n,name:s,disabled:f,label:c,tab:S,value:h,mergedClosable:b,trigger:g,$slots:{default:m}}=this,O=c??S;return i(),p("div",{class:R(`${n}-tabs-tab-wrapper`)},[this.internalLeftPadded?(i(),p("div",{key:0,class:R(`${n}-tabs-tab-pad`)},null,2)):v(()=>null),(i(),p("div",ve({key:s,"data-name":s,"data-disabled":f?!0:void 0},ve({class:[`${n}-tabs-tab`,h===s&&`${n}-tabs-tab--active`,f&&`${n}-tabs-tab--disabled`,b&&`${n}-tabs-tab--closable`,e&&`${n}-tabs-tab--addable`,e?this.addTabClass:this.tabClass],onClick:g==="click"?this.activateTab:void 0,onMouseenter:g==="hover"?this.activateTab:void 0,style:e?this.addStyle:this.style},this.internalCreatedByPane?this.tabProps||{}:this.$attrs)),[q("span",{class:R(`${n}-tabs-tab__label`)},[e?(i(),p(F,{key:0},[q("div",{class:R(`${n}-tabs-tab__height-placeholder`)}," ",2),(i(),E(Ge,{clsPrefix:n},{default:()=>(i(),E(aa))},1032,["clsPrefix"]))],64)):(i(),p(F,{key:1},[m?(i(),p(F,{key:0},[v(()=>m())],64)):(i(),p(F,{key:1},[typeof O=="object"?(i(),p(F,{key:0},[v(()=>O)],64)):(i(),p(F,{key:1},[v(()=>Ht(O??s))],64))],64))],64))],2),b&&this.type==="card"?(i(),E(Ft,{key:0,clsPrefix:n,class:R(`${n}-tabs-tab__close`),onClick:this.handleClose,disabled:f},null,8,["clsPrefix","class","onClick","disabled"])):v(()=>null)],16,Ra))],2)}}),Ta=r("tabs",`
 box-sizing: border-box;
 width: 100%;
 display: flex;
 flex-direction: column;
 transition:
 background-color .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
`,[y("&.transition-disabled",[r("tabs-tab",`
 transition: none !important;
 `),r("tabs-nav-scroll-content",`
 transition: none !important;
 `),r("tabs-tab-pad",`
 transition: none !important;
 `)]),l("segment-type",[r("tabs-rail",[y("&.transition-disabled",[r("tabs-capsule",`
 transition: none;
 `)])])]),l("top",[r("tab-pane",`
 padding: var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left);
 `)]),l("left",[r("tab-pane",`
 padding: var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left) var(--n-pane-padding-top);
 `)]),l("left, right",`
 flex-direction: row;
 `,[r("tabs-bar",`
 width: 2px;
 right: 0;
 transition:
 top .2s var(--n-bezier),
 max-height .2s var(--n-bezier),
 background-color .3s var(--n-bezier);
 `),r("tabs-tab",`
 padding: var(--n-tab-padding-vertical); 
 `)]),l("right",`
 flex-direction: row-reverse;
 `,[r("tab-pane",`
 padding: var(--n-pane-padding-left) var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom);
 `),r("tabs-bar",`
 left: 0;
 `)]),l("bottom",`
 flex-direction: column-reverse;
 justify-content: flex-end;
 `,[r("tab-pane",`
 padding: var(--n-pane-padding-bottom) var(--n-pane-padding-right) var(--n-pane-padding-top) var(--n-pane-padding-left);
 `),r("tabs-bar",`
 top: 0;
 `)]),r("tabs-rail",`
 position: relative;
 padding: 3px;
 border-radius: var(--n-tab-border-radius);
 width: 100%;
 background-color: var(--n-color-segment);
 transition: background-color .3s var(--n-bezier);
 display: flex;
 align-items: center;
 `,[r("tabs-capsule",`
 border-radius: var(--n-tab-border-radius);
 position: absolute;
 left: 0;
 top: 0;
 pointer-events: none;
 background-color: var(--n-tab-color-segment);
 box-shadow: 0 1px 3px 0 rgba(0, 0, 0, .08);
 transition: transform 0.3s var(--n-bezier);
 `),r("tabs-tab-wrapper",`
 flex-basis: 0;
 flex-grow: 1;
 display: flex;
 align-items: center;
 justify-content: center;
 `,[r("tabs-tab",`
 overflow: hidden;
 border-radius: var(--n-tab-border-radius);
 width: 100%;
 display: flex;
 align-items: center;
 justify-content: center;
 `,[l("active",`
 font-weight: var(--n-font-weight-strong);
 color: var(--n-tab-text-color-active);
 `),y("&:hover",`
 color: var(--n-tab-text-color-hover);
 `)])])]),l("flex",[r("tabs-nav",`
 width: 100%;
 position: relative;
 `,[r("tabs-wrapper",`
 width: 100%;
 `,[r("tabs-tab",`
 margin-right: 0;
 `)])])]),r("tabs-nav",`
 box-sizing: border-box;
 line-height: 1.5;
 display: flex;
 transition: border-color .3s var(--n-bezier);
 `,[k("prefix, suffix",`
 display: flex;
 align-items: center;
 `),k("prefix","padding-right: 16px;"),k("suffix","padding-left: 16px;")]),l("top, bottom",[y(">",[r("tabs-nav",[r("tabs-nav-scroll-wrapper",[y("&::before",`
 top: 0;
 bottom: 0;
 left: 0;
 width: 20px;
 `),y("&::after",`
 top: 0;
 bottom: 0;
 right: 0;
 width: 20px;
 `),l("shadow-start",[y("&::before",`
 box-shadow: inset 10px 0 8px -8px rgba(0, 0, 0, .12);
 `)]),l("shadow-end",[y("&::after",`
 box-shadow: inset -10px 0 8px -8px rgba(0, 0, 0, .12);
 `)])])])])]),l("left, right",[r("tabs-nav-scroll-content",`
 flex-direction: column;
 `),y(">",[r("tabs-nav",[r("tabs-nav-scroll-wrapper",[y("&::before",`
 top: 0;
 left: 0;
 right: 0;
 height: 20px;
 `),y("&::after",`
 bottom: 0;
 left: 0;
 right: 0;
 height: 20px;
 `),l("shadow-start",[y("&::before",`
 box-shadow: inset 0 10px 8px -8px rgba(0, 0, 0, .12);
 `)]),l("shadow-end",[y("&::after",`
 box-shadow: inset 0 -10px 8px -8px rgba(0, 0, 0, .12);
 `)])])])])]),r("tabs-nav-scroll-wrapper",`
 flex: 1;
 position: relative;
 overflow: hidden;
 `,[r("tabs-nav-y-scroll",`
 height: 100%;
 width: 100%;
 overflow-y: auto; 
 scrollbar-width: none;
 `,[y("&::-webkit-scrollbar, &::-webkit-scrollbar-track-piece, &::-webkit-scrollbar-thumb",`
 width: 0;
 height: 0;
 display: none;
 `)]),y("&::before, &::after",`
 transition: box-shadow .3s var(--n-bezier);
 pointer-events: none;
 content: "";
 position: absolute;
 z-index: 1;
 `),y("&.transition-disabled",[y("&::before, &::after",`
 transition: none;
 `)])]),r("tabs-nav-scroll-content",`
 display: flex;
 position: relative;
 min-width: 100%;
 min-height: 100%;
 width: fit-content;
 box-sizing: border-box;
 `),r("tabs-wrapper",`
 display: inline-flex;
 flex-wrap: nowrap;
 position: relative;
 `),r("tabs-tab-wrapper",`
 display: flex;
 flex-wrap: nowrap;
 flex-shrink: 0;
 flex-grow: 0;
 `),r("tabs-tab",`
 cursor: pointer;
 white-space: nowrap;
 flex-wrap: nowrap;
 display: inline-flex;
 align-items: center;
 color: var(--n-tab-text-color);
 font-size: var(--n-tab-font-size);
 background-clip: padding-box;
 padding: var(--n-tab-padding);
 transition:
 box-shadow .3s var(--n-bezier),
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
 `,[l("disabled",{cursor:"not-allowed"}),k("close",`
 margin-inline-start: 6px;
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),k("label",`
 display: flex;
 align-items: center;
 z-index: 1;
 `)]),r("tabs-bar",`
 position: absolute;
 bottom: 0;
 height: 2px;
 border-radius: 1px;
 background-color: var(--n-bar-color);
 transition:
 left .2s var(--n-bezier),
 max-width .2s var(--n-bezier),
 opacity .3s var(--n-bezier),
 background-color .3s var(--n-bezier);
 `,[y("&.transition-disabled",`
 transition: none;
 `),l("disabled",`
 background-color: var(--n-tab-text-color-disabled)
 `)]),r("tabs-pane-wrapper",`
 position: relative;
 overflow: hidden;
 transition: max-height .2s var(--n-bezier);
 `),r("tab-pane",`
 color: var(--n-pane-text-color);
 width: 100%;
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 opacity .2s var(--n-bezier);
 left: 0;
 right: 0;
 top: 0;
 `,[y("&.next-transition-leave-active, &.prev-transition-leave-active, &.next-transition-enter-active, &.prev-transition-enter-active",`
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 transform .2s var(--n-bezier),
 opacity .2s var(--n-bezier);
 `),y("&.next-transition-leave-active, &.prev-transition-leave-active",`
 position: absolute;
 `),y("&.next-transition-enter-from, &.prev-transition-leave-to",`
 transform: translateX(32px);
 opacity: 0;
 `),y("&.next-transition-leave-to, &.prev-transition-enter-from",`
 transform: translateX(-32px);
 opacity: 0;
 `),y("&.next-transition-leave-from, &.next-transition-enter-to, &.prev-transition-leave-from, &.prev-transition-enter-to",`
 transform: translateX(0);
 opacity: 1;
 `)]),r("tabs-tab-pad",`
 box-sizing: border-box;
 width: var(--n-tab-gap);
 flex-grow: 0;
 flex-shrink: 0;
 `),l("line-type, bar-type",[r("tabs-tab",`
 font-weight: var(--n-tab-font-weight);
 box-sizing: border-box;
 vertical-align: bottom;
 `,[y("&:hover",{color:"var(--n-tab-text-color-hover)"}),l("active",`
 color: var(--n-tab-text-color-active);
 font-weight: var(--n-tab-font-weight-active);
 `),l("disabled",{color:"var(--n-tab-text-color-disabled)"})])]),r("tabs-nav",[k("prefix, suffix",`
 border-color: var(--n-tab-border-color);
 `),r("tabs-nav-scroll-content",`
 border-color: var(--n-tab-border-color);
 `),l("line-type",[l("top",[k("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),r("tabs-nav-scroll-content",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),r("tabs-bar",`
 bottom: -1px;
 `)]),l("left",[k("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),r("tabs-nav-scroll-content",`
 border-right: 1px solid var(--n-tab-border-color);
 `),r("tabs-bar",`
 right: -1px;
 `)]),l("right",[k("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),r("tabs-nav-scroll-content",`
 border-left: 1px solid var(--n-tab-border-color);
 `),r("tabs-bar",`
 left: -1px;
 `)]),l("bottom",[k("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),r("tabs-nav-scroll-content",`
 border-top: 1px solid var(--n-tab-border-color);
 `),r("tabs-bar",`
 top: -1px;
 `)]),k("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),r("tabs-nav-scroll-content",`
 transition: border-color .3s var(--n-bezier);
 `),r("tabs-bar",`
 border-radius: 0;
 `)]),l("card-type",[k("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),r("tabs-pad",`
 flex-grow: 1;
 transition: border-color .3s var(--n-bezier);
 `),r("tabs-tab-pad",`
 transition: border-color .3s var(--n-bezier);
 `),r("tabs-tab",`
 font-weight: var(--n-tab-font-weight);
 border: 1px solid var(--n-tab-border-color);
 background-color: var(--n-tab-color);
 box-sizing: border-box;
 position: relative;
 vertical-align: bottom;
 display: flex;
 justify-content: space-between;
 font-size: var(--n-tab-font-size);
 color: var(--n-tab-text-color);
 `,[l("addable",`
 padding-left: 8px;
 padding-right: 8px;
 font-size: 16px;
 justify-content: center;
 `,[k("height-placeholder",`
 width: 0;
 font-size: var(--n-tab-font-size);
 `),Dt("disabled",[y("&:hover",`
 color: var(--n-tab-text-color-hover);
 `)])]),l("closable","padding-inline-end: 8px;"),l("active",`
 background-color: #0000;
 font-weight: var(--n-tab-font-weight-active);
 color: var(--n-tab-text-color-active);
 `),l("disabled","color: var(--n-tab-text-color-disabled);")])]),l("left, right",`
 flex-direction: column; 
 `,[k("prefix, suffix",`
 padding: var(--n-tab-padding-vertical);
 `),r("tabs-wrapper",`
 flex-direction: column;
 `),r("tabs-tab-wrapper",`
 flex-direction: column;
 `,[r("tabs-tab-pad",`
 height: var(--n-tab-gap-vertical);
 width: 100%;
 `)])]),l("top",[l("card-type",[r("tabs-scroll-padding","border-bottom: 1px solid var(--n-tab-border-color);"),k("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),r("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-top-right-radius: var(--n-tab-border-radius);
 `,[l("active",`
 border-bottom: 1px solid #0000;
 `)]),r("tabs-tab-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),r("tabs-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `)])]),l("left",[l("card-type",[r("tabs-scroll-padding","border-right: 1px solid var(--n-tab-border-color);"),k("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),r("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-bottom-left-radius: var(--n-tab-border-radius);
 `,[l("active",`
 border-right: 1px solid #0000;
 `)]),r("tabs-tab-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `),r("tabs-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `)])]),l("right",[l("card-type",[r("tabs-scroll-padding","border-left: 1px solid var(--n-tab-border-color);"),k("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),r("tabs-tab",`
 border-top-right-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[l("active",`
 border-left: 1px solid #0000;
 `)]),r("tabs-tab-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `),r("tabs-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `)])]),l("bottom",[l("card-type",[r("tabs-scroll-padding","border-top: 1px solid var(--n-tab-border-color);"),k("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),r("tabs-tab",`
 border-bottom-left-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[l("active",`
 border-top: 1px solid #0000;
 `)]),r("tabs-tab-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `),r("tabs-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `)])])]),r("tabs-scroll-button",[l("start",`
 padding-left: 10px;
 padding-right: 6px;
 `),l("end",`
 padding-right: 10px;
 padding-left: 6px;
 `),l("up",`
 padding-bottom: 10px;
 `),l("down",`
 padding-top: 10px;
 `)])]),Me=ee({name:"TabsButton",props:{type:{type:String,default:"next"},mergedClsPrefix:{type:String,required:!0},vertical:Boolean,disabled:Boolean,rtl:Boolean,theme:Object,themeOverrides:Object,onClick:Function},setup(e){return{handleClick:()=>{e.disabled||e.onClick?.(e.type)}}},render(){const{mergedClsPrefix:e,disabled:n,type:s,vertical:f,rtl:c,theme:S,themeOverrides:h,handleClick:b}=this,g=s==="next",m=f?g:c?!g:g;return i(),E(ea,{text:!0,disabled:n,size:"small",theme:S,themeOverrides:h,onClick:b,class:R([`${e}-tabs-scroll-button`,!f&&s==="prev"&&`${e}-tabs-scroll-button--start`,!f&&s==="next"&&`${e}-tabs-scroll-button--end`,f&&s==="prev"&&`${e}-tabs-scroll-button--up`,f&&s==="next"&&`${e}-tabs-scroll-button--down`])},{icon:()=>(i(),E(Ge,{clsPrefix:e,style:Q(f?{transform:"rotate(90deg)"}:void 0)},{default:()=>m?(i(),E(wa,{key:1})):(i(),E(Ca,{key:2}))},1032,["clsPrefix","style"]))},1032,["disabled","theme","themeOverrides","onClick","class"])}});const Se=ma,za={...Ye.props,value:[String,Number],defaultValue:[String,Number],trigger:{type:String,default:"click"},type:{type:String,default:"bar"},closable:Boolean,justifyContent:String,size:String,placement:{type:String,default:"top"},tabStyle:[String,Object],tabClass:String,addTabStyle:[String,Object],addTabClass:String,barWidth:Number,paneClass:String,paneStyle:[String,Object],paneWrapperClass:String,paneWrapperStyle:[String,Object],addable:[Boolean,Object],tabsPadding:{type:Number,default:0},animated:Boolean,onBeforeLeave:Function,onAdd:Function,"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],onClose:[Function,Array],labelSize:String,activeName:[String,Number],onActiveNameChange:[Function,Array],showScrollButton:Boolean,centerActiveTab:Boolean};var Wa=ee({name:"Tabs",props:za,slots:Object,setup(e,{slots:n}){const{mergedClsPrefixRef:s,inlineThemeDisabled:f,mergedComponentPropsRef:c,mergedRtlRef:S}=Nt(e),h=Vt("Tabs",S,s),b=ne(()=>{const{placement:t}=e;return t==="start"?h?.value?"right":"left":t==="end"?h?.value?"left":"right":t}),g=Ye("Tabs","-tabs",Ta,Yt,e,s),m=I(null),O=I(null),B=I(null),A=I(null),j=I(null),L=I(null),z=I(null),M=I(!0),_=I(!0),K=je(e,["labelSize","size"]),J=ne(()=>{if(K.value)return K.value;const t=c?.value?.Tabs?.size;return t||"medium"}),U=je(e,["activeName","value"]),D=I(U.value??e.defaultValue??(n.default?we(n.default())[0]?.props?.name:null)),u=ta(U,D),$={id:0},N=ne(()=>{if(!(!e.justifyContent||e.type==="card"))return{display:"flex",justifyContent:e.justifyContent}});oe(u,()=>{$.id=0,W(),ie(()=>{he()})});function H(){const{value:t}=u;return t===null?null:m.value?.querySelector(`[data-name="${t}"]`)}function se(t){if(e.type==="card")return;const{value:a}=B;if(!a)return;const o=a.style.opacity==="0";if(t){const d=`${s.value}-tabs-bar--disabled`,{barWidth:x}=e,T=b.value;if(t.dataset.disabled==="true"?a.classList.add(d):a.classList.remove(d),["top","bottom"].includes(T)){if(C(["top","maxHeight","height"]),typeof x=="number"&&t.offsetWidth>=x){const w=Math.floor((t.offsetWidth-x)/2)+t.offsetLeft;a.style.left=`${w}px`,a.style.maxWidth=`${x}px`}else a.style.left=`${t.offsetLeft}px`,a.style.maxWidth=`${t.offsetWidth}px`;a.style.width="8192px",o&&(a.style.transition="none"),a.offsetWidth,o&&(a.style.transition="",a.style.opacity="1")}else{if(C(["left","maxWidth","width"]),typeof x=="number"&&t.offsetHeight>=x){const w=Math.floor((t.offsetHeight-x)/2)+t.offsetTop;a.style.top=`${w}px`,a.style.maxHeight=`${x}px`}else a.style.top=`${t.offsetTop}px`,a.style.maxHeight=`${t.offsetHeight}px`;a.style.height="8192px",o&&(a.style.transition="none"),a.offsetHeight,o&&(a.style.transition="",a.style.opacity="1")}}}function V(){if(e.type==="card")return;const{value:t}=B;t&&(t.style.opacity="0")}function C(t){const{value:a}=B;if(a)for(const o of t)a.style[o]=""}function W(){if(e.type==="card")return;const t=H();t?se(t):V()}function te(t,a,o,d){const x=t.getBoundingClientRect(),T=a.getBoundingClientRect(),w=o?"left":"top",P=o?"right":"bottom";let X=0;d?X=(T[w]+T[P])/2-(x[w]+x[P])/2:T[w]<x[w]?X=T[w]-x[w]:T[P]>x[P]&&(X=T[P]-x[P]),X!==0&&t.scrollBy({[w]:X,behavior:"smooth"})}function he(){const t=["top","bottom"].includes(b.value),a=H();if(a)if(t){const o=L.value?.$el;if(!o)return;te(o,a,t,e.centerActiveTab)}else{const{value:o}=z;if(!o)return;te(o,a,t,e.centerActiveTab)}}const le=I(null);let ge=0,Z=null;function Ke(t){const a=le.value;if(a){ge=t.getBoundingClientRect().height;const o=`${ge}px`,d=()=>{a.style.height=o,a.style.maxHeight=o};Z?(d(),Z(),Z=null):Z=d}}function Ze(t){const a=le.value;if(a){const o=t.getBoundingClientRect().height,d=()=>{document.body.offsetHeight,a.style.maxHeight=`${o}px`,a.style.height=`${Math.max(ge,o)}px`};Z?(Z(),Z=null,d()):Z=d}}function Je(){const t=le.value;if(t){t.style.maxHeight="",t.style.height="";const{paneWrapperStyle:a}=e;if(typeof a=="string")t.style.cssText=a;else if(a){const{maxHeight:o,height:d}=a;o!==void 0&&(t.style.maxHeight=o),d!==void 0&&(t.style.height=d)}}}const Pe={value:[]},ke=I("next");function Qe(t){const a=u.value;let o="next";for(const d of Pe.value){if(d===a)break;if(d===t){o="prev";break}}ke.value=o,et(t)}function et(t){const{onActiveNameChange:a,onUpdateValue:o,"onUpdate:value":d}=e;a&&ue(a,t),o&&ue(o,t),d&&ue(d,t),D.value=t}function tt(t){const{onClose:a}=e;a&&ue(a,t)}function at(t){if(["top","bottom"].includes(b.value)){const{value:a}=L;if(!a)return;const o=a.$el;if(!o)return;const d=o.offsetWidth,x=!!h?.value,T=t==="next"?d:-d;o.scrollBy({left:x?-T:T,behavior:"smooth"})}else{const{value:a}=z;if(!a)return;const o=a.offsetHeight,d=t==="next"?a.scrollTop+o:a.scrollTop-o;a.scrollTo({top:d,left:0,behavior:"smooth"})}}let me=!0;function xe(){const{value:t}=B;if(!t)return;me&&(me=!1);const a="transition-disabled";t.classList.add(a),W(),t.classList.remove(a)}const ae=I(null);function de({transitionDisabled:t}){const a=m.value;if(!a)return;t&&a.classList.add("transition-disabled");const o=H();o&&ae.value&&(ae.value.style.width=`${o.offsetWidth}px`,ae.value.style.height=`${o.offsetHeight}px`,ae.value.style.transform=`translate(${o.offsetLeft}px, ${o.offsetTop}px)`,t&&ae.value.offsetWidth),t&&a.classList.remove("transition-disabled")}oe([u],()=>{e.type==="segment"&&ie(()=>{de({transitionDisabled:!1})})}),Ut(()=>{e.type==="segment"&&de({transitionDisabled:!0})});let Be=0;function rt(t){if(t.contentRect.width===0&&t.contentRect.height===0||Be===t.contentRect.width)return;Be=t.contentRect.width;const{type:a}=e;(a==="line"||a==="bar")&&(me||e.justifyContent?.startsWith("space"))&&xe(),a!=="segment"&&ce(_e())}const nt=Se(rt,64);function Le(){const{type:t}=e;t==="line"||t==="bar"?xe():t==="segment"&&de({transitionDisabled:!0})}oe([()=>e.justifyContent,()=>e.size],()=>{ie(()=>{(e.type==="line"||e.type==="bar")&&xe()})}),oe([b,()=>h?.value],()=>{ie(()=>{Le(),ce(_e(),{instantly:!0})})}),oe(()=>e.type,()=>{ie(()=>{const t=O.value;t&&(t.classList.add("transition-disabled"),Le(),t.offsetWidth,t.classList.remove("transition-disabled"))})});const re=I(!1);function ot(t){const{target:a,contentRect:{width:o,height:d}}=t,x=a.parentElement.parentElement.offsetWidth,T=a.parentElement.parentElement.offsetHeight,w=b.value;if(!re.value)w==="top"||w==="bottom"?x<o&&(re.value=!0):T<d&&(re.value=!0);else{const{value:P}=j;if(!P)return;w==="top"||w==="bottom"?x-o>P.$el.offsetWidth&&(re.value=!1):T-d>P.$el.offsetHeight&&(re.value=!1)}ce(L.value?.$el||null)}const it=Se(ot,64);function st(){const{onAdd:t}=e;t&&t()}const ye=I(!1);function _e(){const t=b.value;return(t==="top"||t==="bottom"?L.value?.$el:z.value)||null}function ce(t,a={instantly:!1}){if(!t)return;const o=a.instantly?A.value:null;o&&o.classList.add("transition-disabled");const d=1,x=b.value;if(x==="top"||x==="bottom"){const{scrollLeft:T,scrollWidth:w,offsetWidth:P}=t,X=Math.abs(T);M.value=X<=d,_.value=X+P>=w-d,ye.value=P<w-d}else{const{scrollTop:T,scrollHeight:w,offsetHeight:P}=t;M.value=T<=d,_.value=T+P>=w-d,ye.value=P<w-d}o&&(o.offsetWidth,o.classList.remove("transition-disabled"))}const lt=Se(t=>{ce(t.target)},64);Qt($e,{triggerRef:Y(e,"trigger"),tabStyleRef:Y(e,"tabStyle"),tabClassRef:Y(e,"tabClass"),addTabStyleRef:Y(e,"addTabStyle"),addTabClassRef:Y(e,"addTabClass"),paneClassRef:Y(e,"paneClass"),paneStyleRef:Y(e,"paneStyle"),mergedClsPrefixRef:s,typeRef:Y(e,"type"),closableRef:Y(e,"closable"),valueRef:u,tabChangeIdRef:$,onBeforeLeaveRef:Y(e,"onBeforeLeave"),activateTab:Qe,handleClose:tt,handleAdd:st}),na(()=>{W(),he()}),Xt(()=>{const{value:t}=A;if(!t)return;const{value:a}=s,o=`${a}-tabs-nav-scroll-wrapper--shadow-start`,d=`${a}-tabs-nav-scroll-wrapper--shadow-end`;M.value?t.classList.remove(o):t.classList.add(o),_.value?t.classList.remove(d):t.classList.add(d)});const dt={syncBarPosition:()=>{W()},scrollToCurrentTab:()=>{he()}},ct=()=>{de({transitionDisabled:!0})},We=ne(()=>{const{value:t}=J,{type:a}=e,o=`${t}${{card:"Card",bar:"Bar",line:"Line",segment:"Segment"}[a]}`,{self:{barColor:d,closeIconColor:x,closeIconColorHover:T,closeIconColorPressed:w,tabColor:P,tabBorderColor:X,paneTextColor:bt,tabFontWeight:ft,tabBorderRadius:ut,tabFontWeightActive:pt,colorSegment:vt,fontWeightStrong:ht,tabColorSegment:gt,closeSize:mt,closeIconSize:xt,closeColorHover:yt,closeColorPressed:Ct,closeBorderRadius:wt,[G("panePadding",t)]:be,[G("tabPadding",o)]:Rt,[G("tabPaddingVertical",o)]:St,[G("tabGap",o)]:Tt,[G("tabGap",`${o}Vertical`)]:zt,[G("tabTextColor",a)]:$t,[G("tabTextColorActive",a)]:Pt,[G("tabTextColorHover",a)]:kt,[G("tabTextColorDisabled",a)]:Bt,[G("tabFontSize",t)]:Lt},common:{cubicBezierEaseInOut:_t}}=g.value;return{"--n-bezier":_t,"--n-color-segment":vt,"--n-bar-color":d,"--n-tab-font-size":Lt,"--n-tab-text-color":$t,"--n-tab-text-color-active":Pt,"--n-tab-text-color-disabled":Bt,"--n-tab-text-color-hover":kt,"--n-pane-text-color":bt,"--n-tab-border-color":X,"--n-tab-border-radius":ut,"--n-close-size":mt,"--n-close-icon-size":xt,"--n-close-color-hover":yt,"--n-close-color-pressed":Ct,"--n-close-border-radius":wt,"--n-close-icon-color":x,"--n-close-icon-color-hover":T,"--n-close-icon-color-pressed":w,"--n-tab-color":P,"--n-tab-font-weight":ft,"--n-tab-font-weight-active":pt,"--n-tab-padding":Rt,"--n-tab-padding-vertical":St,"--n-tab-gap":Tt,"--n-tab-gap-vertical":zt,"--n-pane-padding-left":fe(be,"left"),"--n-pane-padding-right":fe(be,"right"),"--n-pane-padding-top":fe(be,"top"),"--n-pane-padding-bottom":fe(be,"bottom"),"--n-font-weight-strong":ht,"--n-tab-color-segment":gt}}),Ee=f?Gt("tabs",ne(()=>`${J.value[0]}${e.type[0]}`),We,e):void 0;return{mergedClsPrefix:s,mergedValue:u,renderedNames:new Set,segmentCapsuleElRef:ae,tabsPaneWrapperRef:le,tabsElRef:m,selfElRef:O,barElRef:B,addTabInstRef:j,xScrollInstRef:L,scrollWrapperElRef:A,addTabFixed:re,tabWrapperStyle:N,handleNavResize:nt,mergedSize:J,handleScroll:lt,handleTabsResize:it,cssVars:f?void 0:We,themeClass:Ee?.themeClass,animationDirection:ke,renderNameListRef:Pe,yScrollElRef:z,handleSegmentResize:ct,onAnimationBeforeLeave:Ke,onAnimationEnter:Ze,onAnimationAfterEnter:Je,onRender:Ee?.onRender,startReachedRef:M,endReachedRef:_,isOverflow:ye,handleButtonClick:at,mergedTheme:g,rtlEnabled:h,mergedPlacement:b,...dt}},render(){const{mergedClsPrefix:e,type:n,mergedPlacement:s,addTabFixed:f,addable:c,mergedSize:S,renderNameListRef:h,onRender:b,paneWrapperClass:g,paneWrapperStyle:m,startReachedRef:O,endReachedRef:B,isOverflow:A,showScrollButton:j,handleButtonClick:L,mergedTheme:z,rtlEnabled:M,$slots:{default:_,prefix:K,suffix:J}}=this;b?.();const U=_?we(_()).filter(C=>C.type.__TAB_PANE__===!0):[],D=_?we(_()).filter(C=>C.type.__TAB__===!0):[],u=!D.length,$=n==="card",N=n==="segment",H=!$&&!N&&this.justifyContent;h.value=[];const se=()=>{const C=(i(),p("div",{style:Q(this.tabWrapperStyle),class:R(`${e}-tabs-wrapper`)},[H?v(()=>null):(i(),p("div",{key:1,class:R(`${e}-tabs-scroll-padding`),style:Q(s==="top"||s==="bottom"?{width:`${this.tabsPadding}px`}:{height:`${this.tabsPadding}px`})},null,6)),u?(i(),p(F,{key:2},[v(()=>U.map((W,te)=>(h.value.push(W.props.name),Te((i(),E(ze,ve(W.props,{internalCreatedByPane:!0,internalLeftPadded:te!==0&&(!H||H==="center"||H==="start"||H==="end")}),Ae(W.children?{default:W.children.tab}:void 0),1040,["internalLeftPadded"]))))))],64)):(i(),p(F,{key:3},[v(()=>D.map((W,te)=>(h.value.push(W.props.name),Te(te!==0&&!H?Ve(W):W))))],64)),!f&&c&&$?(i(),p(F,{key:4},[v(()=>Ne(c,(u?U.length:D.length)!==0))],64)):v(()=>null),H?v(()=>null):(i(),p("div",{key:7,class:R(`${e}-tabs-scroll-padding`),style:Q({width:`${this.tabsPadding}px`})},null,6)),$?v(()=>null):(i(),p("div",{key:9,ref:"barElRef",class:R(`${e}-tabs-bar`)},null,2))],6));return i(),p("div",{ref:"tabsElRef",class:R(`${e}-tabs-nav-scroll-content`)},[$&&c?(i(),E(Ce,{key:0,onResize:this.handleTabsResize},{default:()=>C},1032,["onResize"])):(i(),p(F,{key:1},[v(()=>C)],64)),$?(i(),p("div",{key:2,class:R(`${e}-tabs-pad`)},null,2)):v(()=>null)],2)},V=N?"top":s;return i(),p("div",{ref:"selfElRef",class:R([`${e}-tabs`,this.themeClass,`${e}-tabs--${n}-type`,`${e}-tabs--${S}-size`,H&&`${e}-tabs--flex`,`${e}-tabs--${V}`,M&&`${e}-tabs--rtl`]),style:Q(this.cssVars)},[q("div",{class:R([`${e}-tabs-nav--${n}-type`,`${e}-tabs-nav--${V}`,`${e}-tabs-nav`])},[v(()=>Ie(K,C=>C&&(i(),p("div",{class:R(`${e}-tabs-nav__prefix`)},[v(()=>C)],2)))),N?(i(),E(Ce,{key:0,onResize:this.handleSegmentResize},{default:()=>(i(),p("div",{class:R(`${e}-tabs-rail`),ref:"tabsElRef"},[q("div",{class:R(`${e}-tabs-capsule`),ref:"segmentCapsuleElRef"},[q("div",{class:R(`${e}-tabs-wrapper`)},[q("div",{class:R(`${e}-tabs-tab`)},null,2)],2)],2),u?(i(),p(F,{key:0},[v(()=>U.map((C,W)=>(h.value.push(C.props.name),i(),E(ze,ve(C.props,{internalCreatedByPane:!0,internalLeftPadded:W!==0}),Ae(C.children?{default:C.children.tab}:void 0),1040,["internalLeftPadded"]))))],64)):(i(),p(F,{key:1},[v(()=>D.map((C,W)=>(h.value.push(C.props.name),W===0?C:Ve(C))))],64))],2))},1032,["onResize"])):(i(),p(F,{key:1},[v(()=>j&&A&&(i(),E(Me,{mergedClsPrefix:e,type:"prev",vertical:V==="left"||V==="right",disabled:O,rtl:!!M,theme:z.peers.Button,themeOverrides:z.peerOverrides.Button,onClick:L},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"]))),(i(),E(Ce,{onResize:this.handleNavResize},{default:()=>(i(),p("div",{class:R(`${e}-tabs-nav-scroll-wrapper`),ref:"scrollWrapperElRef"},[["top","bottom"].includes(V)?(i(),E(ya,{key:0,ref:"xScrollInstRef",onScroll:this.handleScroll},{default:se},1032,["onScroll"])):(i(),p("div",{key:1,class:R(`${e}-tabs-nav-y-scroll`),onScroll:this.handleScroll,ref:"yScrollElRef"},[v(()=>se())],42,["onScroll"]))],2))},1032,["onResize"])),v(()=>j&&A&&(i(),E(Me,{mergedClsPrefix:e,type:"next",vertical:V==="left"||V==="right",disabled:B,rtl:!!M,theme:z.peers.Button,themeOverrides:z.peerOverrides.Button,onClick:L},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"])))],64)),f&&c&&$?(i(),p(F,{key:2},[v(()=>Ne(c,!0))],64)):v(()=>null),v(()=>Ie(J,C=>C&&(i(),p("div",{class:R(`${e}-tabs-nav__suffix`)},[v(()=>C)],2))))],2),v(()=>u&&(this.animated&&(V==="top"||V==="bottom")?(i(),p("div",{key:1,ref:"tabsPaneWrapperRef",style:Q(m),class:R([`${e}-tabs-pane-wrapper`,g])},[v(()=>De(U,this.mergedValue,this.renderedNames,this.onAnimationBeforeLeave,this.onAnimationEnter,this.onAnimationAfterEnter,this.animationDirection))],6)):De(U,this.mergedValue,this.renderedNames)))],6)}});function De(e,n,s,f,c,S,h){const b=[];return e.forEach(g=>{const{name:m,displayDirective:O,"display-directive":B}=g.props,A=L=>O===L||B===L,j=n===m;if(g.key!==void 0&&(g.key=m),j||A("show")||A("show:lazy")&&s.has(m)){s.has(m)||s.add(m);const L=!A("if");b.push(L?qt(g,[[Kt,j]]):g)}}),h?(i(),E(Zt,{name:`${h}-transition`,onBeforeLeave:f,onEnter:c,onAfterEnter:S},{default:()=>b},1032,["name","onBeforeLeave","onEnter","onAfterEnter"])):b}function Ne(e,n){return i(),E(ze,{ref:"addTabInstRef",key:"__addable",name:"__addable",internalCreatedByPane:!0,internalAddable:!0,internalLeftPadded:n,disabled:typeof e=="object"&&e.disabled},null,8,["internalLeftPadded","disabled"])}function Ve(e){const n=Jt(e);return n.props?n.props.internalLeftPadded=!0:n.props={internalLeftPadded:!0},n}function Te(e){return Array.isArray(e.dynamicProps)?e.dynamicProps.includes("internalLeftPadded")||e.dynamicProps.push("internalLeftPadded"):e.dynamicProps=["internalLeftPadded"],e}export{Wa as T,_a as a};
