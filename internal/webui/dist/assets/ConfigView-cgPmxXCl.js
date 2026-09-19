import{u as kt,f as Je,S as lt,I as $e,E as ra}from"./Space-BU7DN7c0.js";import{r as Qe,o as na,a as oa,c as Se,B as le,V as qe,g as Ue,u as la,C as sa,T as Ke}from"./use-message-C58DTaNH.js";import{q as ia,s as He,v as da,d as he,x as ca,y as ua,r as A,z as We,a as b,A as S,B as o,C as u,D,E as ba,G as fa,H as va,I as Me,g as i,c as p,J as T,K as E,b as q,L as pa,M as me,N as tt,O as wt,P as ha,Q as Ct,S as St,n as ce,T as ga,U as ma,V as se,W as de,X as _t,Y as ya,Z as Ve,F as Q,e as O,_ as xa,$ as ka,a0 as wa,a1 as Rt,a2 as Ca,j as Ie,o as zt,a3 as Sa,a4 as _a,a5 as Ra,a6 as st,a7 as Ae,a8 as za,a9 as Ta,aa as $a,ab as Pa,i as Ba,w as X,u as _,ac as Ea,k as it,h as ae,t as re,m as ge,l as De,p as dt,f as ve}from"./index-D2GZLkYI.js";import{c as Oa,a as ct,o as La,u as ut,S as Ge}from"./Select-uHv7ZLYZ.js";import{A as Ia,I as bt}from"./InputNumber-OjJg63jo.js";var Aa=/\s/;function Wa(e){for(var n=e.length;n--&&Aa.test(e.charAt(n)););return n}var ja=/^\s+/;function Na(e){return e&&e.slice(0,Wa(e)+1).replace(ja,"")}var ft=NaN,Ua=/^[-+]0x[0-9a-f]+$/i,Da=/^0b[01]+$/i,Ha=/^0o[0-7]+$/i,Va=parseInt;function vt(e){if(typeof e=="number")return e;if(ia(e))return ft;if(He(e)){var n=typeof e.valueOf=="function"?e.valueOf():e;e=He(n)?n+"":n}if(typeof e!="string")return e===0?e:+e;e=Na(e);var d=Da.test(e);return d||Ha.test(e)?Va(e.slice(2),d?2:8):Ua.test(e)?ft:+e}var Xe=function(){return da.Date.now()},Ma="Expected a function",Fa=Math.max,Ja=Math.min;function qa(e,n,d){var x,h,f,C,g,v,y=0,P=!1,W=!1,L=!0;if(typeof e!="function")throw new TypeError(Ma);n=vt(n)||0,He(d)&&(P=!!d.leading,W="maxWait"in d,f=W?Fa(vt(d.maxWait)||0,n):f,L="trailing"in d?!!d.trailing:L);function U(k){var m=x,V=h;return x=h=void 0,y=k,C=e.apply(V,m),C}function $(k){return y=k,g=setTimeout(F,n),P?U(k):C}function I(k){var m=k-v,V=k-y,B=n-m;return W?Ja(B,f-V):B}function Y(k){var m=k-v,V=k-y;return v===void 0||m>=n||m<0||W&&V>=f}function F(){var k=Xe();if(Y(k))return j(k);g=setTimeout(F,I(k))}function j(k){return g=void 0,L&&x?U(k):(x=h=void 0,C)}function ie(){g!==void 0&&clearTimeout(g),y=0,x=v=h=g=void 0}function te(){return g===void 0?C:j(Xe())}function ee(){var k=Xe(),m=Y(k);if(x=arguments,h=this,v=k,m){if(g===void 0)return $(v);if(W)return clearTimeout(g),g=setTimeout(F,n),U(v)}return g===void 0&&(g=setTimeout(F,n)),C}return ee.cancel=ie,ee.flush=te,ee}var Ka="Expected a function";function Ga(e,n,d){var x=!0,h=!0;if(typeof e!="function")throw new TypeError(Ka);return He(d)&&(x="leading"in d?!!d.leading:x,h="trailing"in d?!!d.trailing:h),qa(e,n,{leading:x,maxWait:n,trailing:h})}const Xa=ct(".v-x-scroll",{overflow:"auto",scrollbarWidth:"none"},[ct("&::-webkit-scrollbar",{width:0,height:0})]),Ya=he({name:"XScroll",props:{disabled:Boolean,onScroll:Function},setup(){const e=A(null);function n(h){!(h.currentTarget.offsetWidth<h.currentTarget.scrollWidth)||h.deltaY===0||(h.currentTarget.scrollLeft+=h.deltaY+h.deltaX,h.preventDefault())}const d=ua();return Xa.mount({id:"vueuc/x-scroll",head:!0,anchorMetaName:Oa,ssr:d}),Object.assign({selfRef:e,handleWheel:n},{scrollTo(...h){var f;(f=e.value)===null||f===void 0||f.scrollTo(...h)}})},render(){return ca("div",{ref:"selfRef",onScroll:this.onScroll,onWheel:this.disabled?void 0:this.handleWheel,class:"v-x-scroll"},this.$slots)}});var Za=he({name:"ChevronLeft",render(){return(()=>{const e=We("dfe229c2639b2082");return e[0]||(e[0]=b("svg",{viewBox:"0 0 16 16",fill:"none",xmlns:"http://www.w3.org/2000/svg"},[b("path",{d:"M10.3536 3.14645C10.5488 3.34171 10.5488 3.65829 10.3536 3.85355L6.20711 8L10.3536 12.1464C10.5488 12.3417 10.5488 12.6583 10.3536 12.8536C10.1583 13.0488 9.84171 13.0488 9.64645 12.8536L5.14645 8.35355C4.95118 8.15829 4.95118 7.84171 5.14645 7.64645L9.64645 3.14645C9.84171 2.95118 10.1583 2.95118 10.3536 3.14645Z",fill:"currentColor"})],-1))})()}}),Qa=he({name:"ChevronRight",render(){return(()=>{const e=We("6ab04425f4fcb756");return e[0]||(e[0]=b("svg",{viewBox:"0 0 16 16",fill:"none",xmlns:"http://www.w3.org/2000/svg"},[b("path",{d:"M5.64645 3.14645C5.45118 3.34171 5.45118 3.65829 5.64645 3.85355L9.79289 8L5.64645 12.1464C5.45118 12.3417 5.45118 12.6583 5.64645 12.8536C5.84171 13.0488 6.15829 13.0488 6.35355 12.8536L10.8536 8.35355C11.0488 8.15829 11.0488 7.84171 10.8536 7.64645L6.35355 3.14645C6.15829 2.95118 5.84171 2.95118 5.64645 3.14645Z",fill:"currentColor"})],-1))})()}}),er=()=>(()=>{const e=We("75be776d8875fa17");return e[0]||(e[0]=b("svg",{viewBox:"0 0 64 64",class:"check-icon"},[b("path",{d:"M50.42,16.76L22.34,39.45l-8.1-11.46c-1.12-1.58-3.3-1.96-4.88-0.84c-1.58,1.12-1.95,3.3-0.84,4.88l10.26,14.51  c0.56,0.79,1.42,1.31,2.38,1.45c0.16,0.02,0.32,0.03,0.48,0.03c0.8,0,1.57-0.27,2.2-0.78l30.99-25.03c1.5-1.21,1.74-3.42,0.52-4.92  C54.13,15.78,51.93,15.55,50.42,16.76z"})],-1))})(),tr=()=>(()=>{const e=We("c6eed899356c8404");return e[0]||(e[0]=b("svg",{viewBox:"0 0 100 100",class:"line-icon"},[b("path",{d:"M80.2,55.5H21.4c-2.8,0-5.1-2.5-5.1-5.5l0,0c0-3,2.3-5.5,5.1-5.5h58.7c2.8,0,5.1,2.5,5.1,5.5l0,0C85.2,53.1,82.9,55.5,80.2,55.5z"})],-1))})(),ar=S([o("checkbox",`
 font-size: var(--n-font-size);
 outline: none;
 cursor: pointer;
 display: inline-flex;
 flex-wrap: nowrap;
 align-items: flex-start;
 word-break: break-word;
 line-height: var(--n-size);
 --n-merged-color-table: var(--n-color-table);
 `,[u("show-label","line-height: var(--n-label-line-height);"),S("&:hover",[o("checkbox-box",[D("border","border: var(--n-border-checked);")])]),S("&:focus:not(:active)",[o("checkbox-box",[D("border",`
 border: var(--n-border-focus);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),u("inside-table",[o("checkbox-box",`
 background-color: var(--n-merged-color-table);
 `)]),u("checked",[o("checkbox-box",`
 background-color: var(--n-color-checked);
 `,[o("checkbox-icon",[S(".check-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),u("indeterminate",[o("checkbox-box",[o("checkbox-icon",[S(".check-icon",`
 opacity: 0;
 transform: scale(.5);
 `),S(".line-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),u("checked, indeterminate",[S("&:focus:not(:active)",[o("checkbox-box",[D("border",`
 border: var(--n-border-checked);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),o("checkbox-box",`
 background-color: var(--n-color-checked);
 border-left: 0;
 border-top: 0;
 `,[D("border",{border:"var(--n-border-checked)"})])]),u("disabled",{cursor:"not-allowed"},[u("checked",[o("checkbox-box",`
 background-color: var(--n-color-disabled-checked);
 `,[D("border",{border:"var(--n-border-disabled-checked)"}),o("checkbox-icon",[S(".check-icon, .line-icon",{fill:"var(--n-check-mark-color-disabled-checked)"})])])]),o("checkbox-box",`
 background-color: var(--n-color-disabled);
 `,[D("border",`
 border: var(--n-border-disabled);
 `),o("checkbox-icon",[S(".check-icon, .line-icon",`
 fill: var(--n-check-mark-color-disabled);
 `)])]),D("label",`
 color: var(--n-text-color-disabled);
 `)]),o("checkbox-box-wrapper",`
 position: relative;
 width: var(--n-size);
 flex-shrink: 0;
 flex-grow: 0;
 user-select: none;
 -webkit-user-select: none;
 `),o("checkbox-box",`
 position: absolute;
 left: 0;
 top: 50%;
 transform: translateY(-50%);
 height: var(--n-size);
 width: var(--n-size);
 display: inline-block;
 box-sizing: border-box;
 border-radius: var(--n-border-radius);
 background-color: var(--n-color);
 transition: background-color 0.3s var(--n-bezier);
 `,[D("border",`
 transition:
 border-color .3s var(--n-bezier),
 box-shadow .3s var(--n-bezier);
 border-radius: inherit;
 position: absolute;
 left: 0;
 right: 0;
 top: 0;
 bottom: 0;
 border: var(--n-border);
 `),o("checkbox-icon",`
 display: flex;
 align-items: center;
 justify-content: center;
 position: absolute;
 left: 1px;
 right: 1px;
 top: 1px;
 bottom: 1px;
 `,[S(".check-icon, .line-icon",`
 width: 100%;
 fill: var(--n-check-mark-color);
 opacity: 0;
 transform: scale(0.5);
 transform-origin: center;
 transition:
 fill 0.3s var(--n-bezier),
 transform 0.3s var(--n-bezier),
 opacity 0.3s var(--n-bezier),
 border-color 0.3s var(--n-bezier);
 `),ba({left:"1px",top:"1px"})])]),D("label",`
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 user-select: none;
 -webkit-user-select: none;
 padding: var(--n-label-padding);
 font-weight: var(--n-label-font-weight);
 `,[S("&:empty",{display:"none"})])]),fa(o("checkbox",`
 --n-merged-color-table: var(--n-color-table-modal);
 `)),va(o("checkbox",`
 --n-merged-color-table: var(--n-color-table-popover);
 `))]);const rr=["id"],nr=["tabindex","aria-checked","aria-labelledby","onKeyup","onKeydown","onClick"],or={...Me.props,size:String,checked:{type:[Boolean,String,Number],default:void 0},defaultChecked:{type:[Boolean,String,Number],default:!1},value:[String,Number],disabled:{type:Boolean,default:void 0},indeterminate:Boolean,label:String,focusable:{type:Boolean,default:!0},checkedValue:{type:[Boolean,String,Number],default:!0},uncheckedValue:{type:[Boolean,String,Number],default:!1},"onUpdate:checked":[Function,Array],onUpdateChecked:[Function,Array],privateInsideTable:Boolean,onChange:[Function,Array]};var pt=he({name:"Checkbox",props:or,setup(e){const n=tt(lr,null),d=A(null),{mergedClsPrefixRef:x,inlineThemeDisabled:h,mergedRtlRef:f,mergedComponentPropsRef:C}=wt(e),g=A(e.defaultChecked),v=de(e,"checked"),y=kt(v,g),P=ha(()=>{if(n){const m=n.valueSetRef.value;return m&&e.value!==void 0?m.has(e.value):!1}else return y.value===e.checkedValue}),W=oa(e,{mergedSize(m){const{size:V}=e;if(V!==void 0)return V;if(n){const{value:K}=n.mergedSizeRef;if(K!==void 0)return K}if(m){const{mergedSize:K}=m;if(K!==void 0)return K.value}const B=C?.value?.Checkbox?.size;return B||"medium"},mergedDisabled(m){const{disabled:V}=e;if(V!==void 0)return V;if(n){if(n.disabledRef.value)return!0;const{maxRef:{value:B},checkedCountRef:K}=n;if(B!==void 0&&K.value>=B&&!P.value)return!0;const{minRef:{value:G}}=n;if(G!==void 0&&K.value<=G&&P.value)return!0}return m?m.disabled.value:!1}}),{mergedDisabledRef:L,mergedSizeRef:U}=W,$=Me("Checkbox","-checkbox",ar,ma,e,x);function I(m){if(n&&e.value!==void 0)n.toggleCheckbox(!P.value,e.value);else{const{onChange:V,"onUpdate:checked":B,onUpdateChecked:K}=e,{nTriggerFormInput:G,nTriggerFormChange:z}=W,N=P.value?e.uncheckedValue:e.checkedValue;B&&Se(B,N,m),K&&Se(K,N,m),V&&Se(V,N,m),G(),z(),g.value=N}}function Y(m){L.value||I(m)}function F(m){if(!L.value)switch(m.key){case" ":case"Enter":I(m)}}function j(m){m.key===" "&&m.preventDefault()}const ie={focus:()=>{d.value?.focus()},blur:()=>{d.value?.blur()}},te=Ct("Checkbox",f,x),ee=ce(()=>{const{value:m}=U,{common:{cubicBezierEaseInOut:V},self:{borderRadius:B,color:K,colorChecked:G,colorDisabled:z,colorTableHeader:N,colorTableHeaderModal:ue,colorTableHeaderPopover:ye,checkMarkColor:ne,checkMarkColorDisabled:pe,border:oe,borderFocus:xe,borderDisabled:be,borderChecked:Pe,boxShadowFocus:_e,textColor:Re,textColorDisabled:Be,checkMarkColorDisabledChecked:Ee,colorDisabledChecked:Oe,borderDisabledChecked:Le,labelPadding:ke,labelLineHeight:r,labelFontWeight:t,[se("fontSize",m)]:l,[se("size",m)]:R}}=$.value;return{"--n-label-line-height":r,"--n-label-font-weight":t,"--n-size":R,"--n-bezier":V,"--n-border-radius":B,"--n-border":oe,"--n-border-checked":Pe,"--n-border-focus":xe,"--n-border-disabled":be,"--n-border-disabled-checked":Le,"--n-box-shadow-focus":_e,"--n-color":K,"--n-color-checked":G,"--n-color-table":N,"--n-color-table-modal":ue,"--n-color-table-popover":ye,"--n-color-disabled":z,"--n-color-disabled-checked":Oe,"--n-text-color":Re,"--n-text-color-disabled":Be,"--n-check-mark-color":ne,"--n-check-mark-color-disabled":pe,"--n-check-mark-color-disabled-checked":Ee,"--n-font-size":l,"--n-label-padding":ke}}),k=h?St("checkbox",ce(()=>U.value[0]),ee,e):void 0;return Object.assign(W,ie,{rtlEnabled:te,selfRef:d,mergedClsPrefix:x,mergedDisabled:L,renderedChecked:P,mergedTheme:$,labelId:ga(),handleClick:Y,handleKeyUp:F,handleKeyDown:j,cssVars:h?void 0:ee,themeClass:k?.themeClass,onRender:k?.onRender})},render(){const{$slots:e,renderedChecked:n,mergedDisabled:d,indeterminate:x,privateInsideTable:h,cssVars:f,labelId:C,label:g,mergedClsPrefix:v,focusable:y,handleKeyUp:P,handleKeyDown:W,handleClick:L}=this;this.onRender?.();const U=Qe(e.default,$=>g||$?(i(),p("span",{key:1,class:E(`${v}-checkbox__label`),id:C},[T(()=>g||$)],10,rr)):null);return(()=>{const $=We("70be6e74cd27cb50");return i(),p("div",{ref:"selfRef",class:E([`${v}-checkbox`,this.themeClass,this.rtlEnabled&&`${v}-checkbox--rtl`,n&&`${v}-checkbox--checked`,d&&`${v}-checkbox--disabled`,x&&`${v}-checkbox--indeterminate`,h&&`${v}-checkbox--inside-table`,U&&`${v}-checkbox--show-label`]),tabindex:d||!y?void 0:0,role:"checkbox","aria-checked":x?"mixed":n,"aria-labelledby":C,style:me(f),onKeyup:P,onKeydown:W,onClick:L,onMousedown:$[0]||($[0]=()=>{na("selectstart",window,I=>{I.preventDefault()},{once:!0})})},[b("div",{class:E(`${v}-checkbox-box-wrapper`)},[$[1]||($[1]=T(" ",-1)),b("div",{class:E(`${v}-checkbox-box`)},[q(pa,null,{default:()=>this.indeterminate?(i(),p("div",{key:"indeterminate",class:E(`${v}-checkbox-icon`)},[T(()=>tr())],2)):(i(),p("div",{key:"check",class:E(`${v}-checkbox-icon`)},[T(()=>er())],2))},1024),b("div",{class:E(`${v}-checkbox-box__border`)},null,2)],2)],2),T(()=>U)],46,nr)})()}});const lr=_t("n-checkbox-group"),at=_t("n-tabs"),Tt={tab:[String,Number,Object,Function],name:{type:[String,Number],required:!0},disabled:Boolean,displayDirective:{type:String,default:"if"},closable:{type:Boolean,default:void 0},tabProps:Object,label:[String,Number,Object,Function]};var ht=he({__TAB_PANE__:!0,name:"TabPane",alias:["TabPanel"],props:Tt,slots:Object,setup(e){const n=tt(at,null);return n||ya("tab-pane","`n-tab-pane` must be placed inside `n-tabs`."),{style:n.paneStyleRef,class:n.paneClassRef,mergedClsPrefix:n.mergedClsPrefixRef}},render(){return i(),p("div",{class:E([`${this.mergedClsPrefix}-tab-pane`,this.class]),style:me(this.style)},[T(()=>this.$slots.default?.())],6)}});const sr=["data-name","data-disabled"],ir={internalLeftPadded:Boolean,internalAddable:Boolean,internalCreatedByPane:Boolean,...wa(Tt,["displayDirective"])};var et=he({__TAB__:!0,inheritAttrs:!1,name:"Tab",props:ir,setup(e){const{mergedClsPrefixRef:n,valueRef:d,typeRef:x,closableRef:h,tabStyleRef:f,addTabStyleRef:C,tabClassRef:g,addTabClassRef:v,tabChangeIdRef:y,onBeforeLeaveRef:P,triggerRef:W,handleAdd:L,activateTab:U,handleClose:$}=tt(at);return{trigger:W,mergedClosable:ce(()=>{if(e.internalAddable)return!1;const{closable:I}=e;return I===void 0?h.value:I}),style:f,addStyle:C,tabClass:g,addTabClass:v,clsPrefix:n,value:d,type:x,handleClose(I){I.stopPropagation(),!e.disabled&&$(e.name)},activateTab(){if(e.disabled)return;if(e.internalAddable){L();return}const{name:I}=e,Y=++y.id;if(I!==d.value){const{value:F}=P;F?Promise.resolve(F(e.name,d.value)).then(j=>{j&&y.id===Y&&U(I)}):U(I)}}}},render(){const{internalAddable:e,clsPrefix:n,name:d,disabled:x,label:h,tab:f,value:C,mergedClosable:g,trigger:v,$slots:{default:y}}=this,P=h??f;return i(),p("div",{class:E(`${n}-tabs-tab-wrapper`)},[this.internalLeftPadded?(i(),p("div",{key:0,class:E(`${n}-tabs-tab-pad`)},null,2)):T(()=>null),(i(),p("div",Ve({key:d,"data-name":d,"data-disabled":x?!0:void 0},Ve({class:[`${n}-tabs-tab`,C===d&&`${n}-tabs-tab--active`,x&&`${n}-tabs-tab--disabled`,g&&`${n}-tabs-tab--closable`,e&&`${n}-tabs-tab--addable`,e?this.addTabClass:this.tabClass],onClick:v==="click"?this.activateTab:void 0,onMouseenter:v==="hover"?this.activateTab:void 0,style:e?this.addStyle:this.style},this.internalCreatedByPane?this.tabProps||{}:this.$attrs)),[b("span",{class:E(`${n}-tabs-tab__label`)},[e?(i(),p(Q,{key:0},[b("div",{class:E(`${n}-tabs-tab__height-placeholder`)}," ",2),(i(),O(Rt,{clsPrefix:n},{default:()=>(i(),O(Ia))},1032,["clsPrefix"]))],64)):(i(),p(Q,{key:1},[y?(i(),p(Q,{key:0},[T(()=>y())],64)):(i(),p(Q,{key:1},[typeof P=="object"?(i(),p(Q,{key:0},[T(()=>P)],64)):(i(),p(Q,{key:1},[T(()=>xa(P??d))],64))],64))],64))],2),g&&this.type==="card"?(i(),O(ka,{key:0,clsPrefix:n,class:E(`${n}-tabs-tab__close`),onClick:this.handleClose,disabled:x},null,8,["clsPrefix","class","onClick","disabled"])):T(()=>null)],16,sr))],2)}}),dr=o("tabs",`
 box-sizing: border-box;
 width: 100%;
 display: flex;
 flex-direction: column;
 transition:
 background-color .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
`,[S("&.transition-disabled",[o("tabs-tab",`
 transition: none !important;
 `),o("tabs-nav-scroll-content",`
 transition: none !important;
 `),o("tabs-tab-pad",`
 transition: none !important;
 `)]),u("segment-type",[o("tabs-rail",[S("&.transition-disabled",[o("tabs-capsule",`
 transition: none;
 `)])])]),u("top",[o("tab-pane",`
 padding: var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left);
 `)]),u("left",[o("tab-pane",`
 padding: var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left) var(--n-pane-padding-top);
 `)]),u("left, right",`
 flex-direction: row;
 `,[o("tabs-bar",`
 width: 2px;
 right: 0;
 transition:
 top .2s var(--n-bezier),
 max-height .2s var(--n-bezier),
 background-color .3s var(--n-bezier);
 `),o("tabs-tab",`
 padding: var(--n-tab-padding-vertical); 
 `)]),u("right",`
 flex-direction: row-reverse;
 `,[o("tab-pane",`
 padding: var(--n-pane-padding-left) var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom);
 `),o("tabs-bar",`
 left: 0;
 `)]),u("bottom",`
 flex-direction: column-reverse;
 justify-content: flex-end;
 `,[o("tab-pane",`
 padding: var(--n-pane-padding-bottom) var(--n-pane-padding-right) var(--n-pane-padding-top) var(--n-pane-padding-left);
 `),o("tabs-bar",`
 top: 0;
 `)]),o("tabs-rail",`
 position: relative;
 padding: 3px;
 border-radius: var(--n-tab-border-radius);
 width: 100%;
 background-color: var(--n-color-segment);
 transition: background-color .3s var(--n-bezier);
 display: flex;
 align-items: center;
 `,[o("tabs-capsule",`
 border-radius: var(--n-tab-border-radius);
 position: absolute;
 left: 0;
 top: 0;
 pointer-events: none;
 background-color: var(--n-tab-color-segment);
 box-shadow: 0 1px 3px 0 rgba(0, 0, 0, .08);
 transition: transform 0.3s var(--n-bezier);
 `),o("tabs-tab-wrapper",`
 flex-basis: 0;
 flex-grow: 1;
 display: flex;
 align-items: center;
 justify-content: center;
 `,[o("tabs-tab",`
 overflow: hidden;
 border-radius: var(--n-tab-border-radius);
 width: 100%;
 display: flex;
 align-items: center;
 justify-content: center;
 `,[u("active",`
 font-weight: var(--n-font-weight-strong);
 color: var(--n-tab-text-color-active);
 `),S("&:hover",`
 color: var(--n-tab-text-color-hover);
 `)])])]),u("flex",[o("tabs-nav",`
 width: 100%;
 position: relative;
 `,[o("tabs-wrapper",`
 width: 100%;
 `,[o("tabs-tab",`
 margin-right: 0;
 `)])])]),o("tabs-nav",`
 box-sizing: border-box;
 line-height: 1.5;
 display: flex;
 transition: border-color .3s var(--n-bezier);
 `,[D("prefix, suffix",`
 display: flex;
 align-items: center;
 `),D("prefix","padding-right: 16px;"),D("suffix","padding-left: 16px;")]),u("top, bottom",[S(">",[o("tabs-nav",[o("tabs-nav-scroll-wrapper",[S("&::before",`
 top: 0;
 bottom: 0;
 left: 0;
 width: 20px;
 `),S("&::after",`
 top: 0;
 bottom: 0;
 right: 0;
 width: 20px;
 `),u("shadow-start",[S("&::before",`
 box-shadow: inset 10px 0 8px -8px rgba(0, 0, 0, .12);
 `)]),u("shadow-end",[S("&::after",`
 box-shadow: inset -10px 0 8px -8px rgba(0, 0, 0, .12);
 `)])])])])]),u("left, right",[o("tabs-nav-scroll-content",`
 flex-direction: column;
 `),S(">",[o("tabs-nav",[o("tabs-nav-scroll-wrapper",[S("&::before",`
 top: 0;
 left: 0;
 right: 0;
 height: 20px;
 `),S("&::after",`
 bottom: 0;
 left: 0;
 right: 0;
 height: 20px;
 `),u("shadow-start",[S("&::before",`
 box-shadow: inset 0 10px 8px -8px rgba(0, 0, 0, .12);
 `)]),u("shadow-end",[S("&::after",`
 box-shadow: inset 0 -10px 8px -8px rgba(0, 0, 0, .12);
 `)])])])])]),o("tabs-nav-scroll-wrapper",`
 flex: 1;
 position: relative;
 overflow: hidden;
 `,[o("tabs-nav-y-scroll",`
 height: 100%;
 width: 100%;
 overflow-y: auto; 
 scrollbar-width: none;
 `,[S("&::-webkit-scrollbar, &::-webkit-scrollbar-track-piece, &::-webkit-scrollbar-thumb",`
 width: 0;
 height: 0;
 display: none;
 `)]),S("&::before, &::after",`
 transition: box-shadow .3s var(--n-bezier);
 pointer-events: none;
 content: "";
 position: absolute;
 z-index: 1;
 `),S("&.transition-disabled",[S("&::before, &::after",`
 transition: none;
 `)])]),o("tabs-nav-scroll-content",`
 display: flex;
 position: relative;
 min-width: 100%;
 min-height: 100%;
 width: fit-content;
 box-sizing: border-box;
 `),o("tabs-wrapper",`
 display: inline-flex;
 flex-wrap: nowrap;
 position: relative;
 `),o("tabs-tab-wrapper",`
 display: flex;
 flex-wrap: nowrap;
 flex-shrink: 0;
 flex-grow: 0;
 `),o("tabs-tab",`
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
 `,[u("disabled",{cursor:"not-allowed"}),D("close",`
 margin-inline-start: 6px;
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),D("label",`
 display: flex;
 align-items: center;
 z-index: 1;
 `)]),o("tabs-bar",`
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
 `,[S("&.transition-disabled",`
 transition: none;
 `),u("disabled",`
 background-color: var(--n-tab-text-color-disabled)
 `)]),o("tabs-pane-wrapper",`
 position: relative;
 overflow: hidden;
 transition: max-height .2s var(--n-bezier);
 `),o("tab-pane",`
 color: var(--n-pane-text-color);
 width: 100%;
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 opacity .2s var(--n-bezier);
 left: 0;
 right: 0;
 top: 0;
 `,[S("&.next-transition-leave-active, &.prev-transition-leave-active, &.next-transition-enter-active, &.prev-transition-enter-active",`
 transition:
 color .3s var(--n-bezier),
 background-color .3s var(--n-bezier),
 transform .2s var(--n-bezier),
 opacity .2s var(--n-bezier);
 `),S("&.next-transition-leave-active, &.prev-transition-leave-active",`
 position: absolute;
 `),S("&.next-transition-enter-from, &.prev-transition-leave-to",`
 transform: translateX(32px);
 opacity: 0;
 `),S("&.next-transition-leave-to, &.prev-transition-enter-from",`
 transform: translateX(-32px);
 opacity: 0;
 `),S("&.next-transition-leave-from, &.next-transition-enter-to, &.prev-transition-leave-from, &.prev-transition-enter-to",`
 transform: translateX(0);
 opacity: 1;
 `)]),o("tabs-tab-pad",`
 box-sizing: border-box;
 width: var(--n-tab-gap);
 flex-grow: 0;
 flex-shrink: 0;
 `),u("line-type, bar-type",[o("tabs-tab",`
 font-weight: var(--n-tab-font-weight);
 box-sizing: border-box;
 vertical-align: bottom;
 `,[S("&:hover",{color:"var(--n-tab-text-color-hover)"}),u("active",`
 color: var(--n-tab-text-color-active);
 font-weight: var(--n-tab-font-weight-active);
 `),u("disabled",{color:"var(--n-tab-text-color-disabled)"})])]),o("tabs-nav",[D("prefix, suffix",`
 border-color: var(--n-tab-border-color);
 `),o("tabs-nav-scroll-content",`
 border-color: var(--n-tab-border-color);
 `),u("line-type",[u("top",[D("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),o("tabs-nav-scroll-content",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),o("tabs-bar",`
 bottom: -1px;
 `)]),u("left",[D("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),o("tabs-nav-scroll-content",`
 border-right: 1px solid var(--n-tab-border-color);
 `),o("tabs-bar",`
 right: -1px;
 `)]),u("right",[D("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),o("tabs-nav-scroll-content",`
 border-left: 1px solid var(--n-tab-border-color);
 `),o("tabs-bar",`
 left: -1px;
 `)]),u("bottom",[D("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),o("tabs-nav-scroll-content",`
 border-top: 1px solid var(--n-tab-border-color);
 `),o("tabs-bar",`
 top: -1px;
 `)]),D("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),o("tabs-nav-scroll-content",`
 transition: border-color .3s var(--n-bezier);
 `),o("tabs-bar",`
 border-radius: 0;
 `)]),u("card-type",[D("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),o("tabs-pad",`
 flex-grow: 1;
 transition: border-color .3s var(--n-bezier);
 `),o("tabs-tab-pad",`
 transition: border-color .3s var(--n-bezier);
 `),o("tabs-tab",`
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
 `,[u("addable",`
 padding-left: 8px;
 padding-right: 8px;
 font-size: 16px;
 justify-content: center;
 `,[D("height-placeholder",`
 width: 0;
 font-size: var(--n-tab-font-size);
 `),Ca("disabled",[S("&:hover",`
 color: var(--n-tab-text-color-hover);
 `)])]),u("closable","padding-inline-end: 8px;"),u("active",`
 background-color: #0000;
 font-weight: var(--n-tab-font-weight-active);
 color: var(--n-tab-text-color-active);
 `),u("disabled","color: var(--n-tab-text-color-disabled);")])]),u("left, right",`
 flex-direction: column; 
 `,[D("prefix, suffix",`
 padding: var(--n-tab-padding-vertical);
 `),o("tabs-wrapper",`
 flex-direction: column;
 `),o("tabs-tab-wrapper",`
 flex-direction: column;
 `,[o("tabs-tab-pad",`
 height: var(--n-tab-gap-vertical);
 width: 100%;
 `)])]),u("top",[u("card-type",[o("tabs-scroll-padding","border-bottom: 1px solid var(--n-tab-border-color);"),D("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),o("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-top-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-bottom: 1px solid #0000;
 `)]),o("tabs-tab-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),o("tabs-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `)])]),u("left",[u("card-type",[o("tabs-scroll-padding","border-right: 1px solid var(--n-tab-border-color);"),D("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),o("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-bottom-left-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-right: 1px solid #0000;
 `)]),o("tabs-tab-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `),o("tabs-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `)])]),u("right",[u("card-type",[o("tabs-scroll-padding","border-left: 1px solid var(--n-tab-border-color);"),D("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),o("tabs-tab",`
 border-top-right-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-left: 1px solid #0000;
 `)]),o("tabs-tab-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `),o("tabs-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `)])]),u("bottom",[u("card-type",[o("tabs-scroll-padding","border-top: 1px solid var(--n-tab-border-color);"),D("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),o("tabs-tab",`
 border-bottom-left-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-top: 1px solid #0000;
 `)]),o("tabs-tab-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `),o("tabs-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `)])])]),o("tabs-scroll-button",[u("start",`
 padding-left: 10px;
 padding-right: 6px;
 `),u("end",`
 padding-right: 10px;
 padding-left: 6px;
 `),u("up",`
 padding-bottom: 10px;
 `),u("down",`
 padding-top: 10px;
 `)])]),gt=he({name:"TabsButton",props:{type:{type:String,default:"next"},mergedClsPrefix:{type:String,required:!0},vertical:Boolean,disabled:Boolean,rtl:Boolean,theme:Object,themeOverrides:Object,onClick:Function},setup(e){return{handleClick:()=>{e.disabled||e.onClick?.(e.type)}}},render(){const{mergedClsPrefix:e,disabled:n,type:d,vertical:x,rtl:h,theme:f,themeOverrides:C,handleClick:g}=this,v=d==="next",y=x?v:h?!v:v;return i(),O(le,{text:!0,disabled:n,size:"small",theme:f,themeOverrides:C,onClick:g,class:E([`${e}-tabs-scroll-button`,!x&&d==="prev"&&`${e}-tabs-scroll-button--start`,!x&&d==="next"&&`${e}-tabs-scroll-button--end`,x&&d==="prev"&&`${e}-tabs-scroll-button--up`,x&&d==="next"&&`${e}-tabs-scroll-button--down`])},{icon:()=>(i(),O(Rt,{clsPrefix:e,style:me(x?{transform:"rotate(90deg)"}:void 0)},{default:()=>y?(i(),O(Qa,{key:1})):(i(),O(Za,{key:2}))},1032,["clsPrefix","style"]))},1032,["disabled","theme","themeOverrides","onClick","class"])}});const Ye=Ga,cr={...Me.props,value:[String,Number],defaultValue:[String,Number],trigger:{type:String,default:"click"},type:{type:String,default:"bar"},closable:Boolean,justifyContent:String,size:String,placement:{type:String,default:"top"},tabStyle:[String,Object],tabClass:String,addTabStyle:[String,Object],addTabClass:String,barWidth:Number,paneClass:String,paneStyle:[String,Object],paneWrapperClass:String,paneWrapperStyle:[String,Object],addable:[Boolean,Object],tabsPadding:{type:Number,default:0},animated:Boolean,onBeforeLeave:Function,onAdd:Function,"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],onClose:[Function,Array],labelSize:String,activeName:[String,Number],onActiveNameChange:[Function,Array],showScrollButton:Boolean,centerActiveTab:Boolean};var ur=he({name:"Tabs",props:cr,slots:Object,setup(e,{slots:n}){const{mergedClsPrefixRef:d,inlineThemeDisabled:x,mergedComponentPropsRef:h,mergedRtlRef:f}=wt(e),C=Ct("Tabs",f,d),g=ce(()=>{const{placement:a}=e;return a==="start"?C?.value?"right":"left":a==="end"?C?.value?"left":"right":a}),v=Me("Tabs","-tabs",dr,Ra,e,d),y=A(null),P=A(null),W=A(null),L=A(null),U=A(null),$=A(null),I=A(null),Y=A(!0),F=A(!0),j=ut(e,["labelSize","size"]),ie=ce(()=>{if(j.value)return j.value;const a=h?.value?.Tabs?.size;return a||"medium"}),te=ut(e,["activeName","value"]),ee=A(te.value??e.defaultValue??(n.default?Je(n.default())[0]?.props?.name:null)),k=kt(te,ee),m={id:0},V=ce(()=>{if(!(!e.justifyContent||e.type==="card"))return{display:"flex",justifyContent:e.justifyContent}});Ie(k,()=>{m.id=0,N(),Ae(()=>{ye()})});function B(){const{value:a}=k;return a===null?null:y.value?.querySelector(`[data-name="${a}"]`)}function K(a){if(e.type==="card")return;const{value:s}=W;if(!s)return;const c=s.style.opacity==="0";if(a){const w=`${d.value}-tabs-bar--disabled`,{barWidth:H}=e,J=g.value;if(a.dataset.disabled==="true"?s.classList.add(w):s.classList.remove(w),["top","bottom"].includes(J)){if(z(["top","maxHeight","height"]),typeof H=="number"&&a.offsetWidth>=H){const M=Math.floor((a.offsetWidth-H)/2)+a.offsetLeft;s.style.left=`${M}px`,s.style.maxWidth=`${H}px`}else s.style.left=`${a.offsetLeft}px`,s.style.maxWidth=`${a.offsetWidth}px`;s.style.width="8192px",c&&(s.style.transition="none"),s.offsetWidth,c&&(s.style.transition="",s.style.opacity="1")}else{if(z(["left","maxWidth","width"]),typeof H=="number"&&a.offsetHeight>=H){const M=Math.floor((a.offsetHeight-H)/2)+a.offsetTop;s.style.top=`${M}px`,s.style.maxHeight=`${H}px`}else s.style.top=`${a.offsetTop}px`,s.style.maxHeight=`${a.offsetHeight}px`;s.style.height="8192px",c&&(s.style.transition="none"),s.offsetHeight,c&&(s.style.transition="",s.style.opacity="1")}}}function G(){if(e.type==="card")return;const{value:a}=W;a&&(a.style.opacity="0")}function z(a){const{value:s}=W;if(s)for(const c of a)s.style[c]=""}function N(){if(e.type==="card")return;const a=B();a?K(a):G()}function ue(a,s,c,w){const H=a.getBoundingClientRect(),J=s.getBoundingClientRect(),M=c?"left":"top",Z=c?"right":"bottom";let fe=0;w?fe=(J[M]+J[Z])/2-(H[M]+H[Z])/2:J[M]<H[M]?fe=J[M]-H[M]:J[Z]>H[Z]&&(fe=J[Z]-H[Z]),fe!==0&&a.scrollBy({[M]:fe,behavior:"smooth"})}function ye(){const a=["top","bottom"].includes(g.value),s=B();if(s)if(a){const c=$.value?.$el;if(!c)return;ue(c,s,a,e.centerActiveTab)}else{const{value:c}=I;if(!c)return;ue(c,s,a,e.centerActiveTab)}}const ne=A(null);let pe=0,oe=null;function xe(a){const s=ne.value;if(s){pe=a.getBoundingClientRect().height;const c=`${pe}px`,w=()=>{s.style.height=c,s.style.maxHeight=c};oe?(w(),oe(),oe=null):oe=w}}function be(a){const s=ne.value;if(s){const c=a.getBoundingClientRect().height,w=()=>{document.body.offsetHeight,s.style.maxHeight=`${c}px`,s.style.height=`${Math.max(pe,c)}px`};oe?(oe(),oe=null,w()):oe=w}}function Pe(){const a=ne.value;if(a){a.style.maxHeight="",a.style.height="";const{paneWrapperStyle:s}=e;if(typeof s=="string")a.style.cssText=s;else if(s){const{maxHeight:c,height:w}=s;c!==void 0&&(a.style.maxHeight=c),w!==void 0&&(a.style.height=w)}}}const _e={value:[]},Re=A("next");function Be(a){const s=k.value;let c="next";for(const w of _e.value){if(w===s)break;if(w===a){c="prev";break}}Re.value=c,Ee(a)}function Ee(a){const{onActiveNameChange:s,onUpdateValue:c,"onUpdate:value":w}=e;s&&Se(s,a),c&&Se(c,a),w&&Se(w,a),ee.value=a}function Oe(a){const{onClose:s}=e;s&&Se(s,a)}function Le(a){if(["top","bottom"].includes(g.value)){const{value:s}=$;if(!s)return;const c=s.$el;if(!c)return;const w=c.offsetWidth,H=!!C?.value,J=a==="next"?w:-w;c.scrollBy({left:H?-J:J,behavior:"smooth"})}else{const{value:s}=I;if(!s)return;const c=s.offsetHeight,w=a==="next"?s.scrollTop+c:s.scrollTop-c;s.scrollTo({top:w,left:0,behavior:"smooth"})}}let ke=!0;function r(){const{value:a}=W;if(!a)return;ke&&(ke=!1);const s="transition-disabled";a.classList.add(s),N(),a.classList.remove(s)}const t=A(null);function l({transitionDisabled:a}){const s=y.value;if(!s)return;a&&s.classList.add("transition-disabled");const c=B();c&&t.value&&(t.value.style.width=`${c.offsetWidth}px`,t.value.style.height=`${c.offsetHeight}px`,t.value.style.transform=`translate(${c.offsetLeft}px, ${c.offsetTop}px)`,a&&t.value.offsetWidth),a&&s.classList.remove("transition-disabled")}Ie([k],()=>{e.type==="segment"&&Ae(()=>{l({transitionDisabled:!1})})}),zt(()=>{e.type==="segment"&&l({transitionDisabled:!0})});let R=0;function we(a){if(a.contentRect.width===0&&a.contentRect.height===0||R===a.contentRect.width)return;R=a.contentRect.width;const{type:s}=e;(s==="line"||s==="bar")&&(ke||e.justifyContent?.startsWith("space"))&&r(),s!=="segment"&&je(rt())}const Ce=Ye(we,64);function ze(){const{type:a}=e;a==="line"||a==="bar"?r():a==="segment"&&l({transitionDisabled:!0})}Ie([()=>e.justifyContent,()=>e.size],()=>{Ae(()=>{(e.type==="line"||e.type==="bar")&&r()})}),Ie([g,()=>C?.value],()=>{Ae(()=>{ze(),je(rt(),{instantly:!0})})}),Ie(()=>e.type,()=>{Ae(()=>{const a=P.value;a&&(a.classList.add("transition-disabled"),ze(),a.offsetWidth,a.classList.remove("transition-disabled"))})});const Te=A(!1);function $t(a){const{target:s,contentRect:{width:c,height:w}}=a,H=s.parentElement.parentElement.offsetWidth,J=s.parentElement.parentElement.offsetHeight,M=g.value;if(!Te.value)M==="top"||M==="bottom"?H<c&&(Te.value=!0):J<w&&(Te.value=!0);else{const{value:Z}=U;if(!Z)return;M==="top"||M==="bottom"?H-c>Z.$el.offsetWidth&&(Te.value=!1):J-w>Z.$el.offsetHeight&&(Te.value=!1)}je($.value?.$el||null)}const Pt=Ye($t,64);function Bt(){const{onAdd:a}=e;a&&a()}const Fe=A(!1);function rt(){const a=g.value;return(a==="top"||a==="bottom"?$.value?.$el:I.value)||null}function je(a,s={instantly:!1}){if(!a)return;const c=s.instantly?L.value:null;c&&c.classList.add("transition-disabled");const w=1,H=g.value;if(H==="top"||H==="bottom"){const{scrollLeft:J,scrollWidth:M,offsetWidth:Z}=a,fe=Math.abs(J);Y.value=fe<=w,F.value=fe+Z>=M-w,Fe.value=Z<M-w}else{const{scrollTop:J,scrollHeight:M,offsetHeight:Z}=a;Y.value=J<=w,F.value=J+Z>=M-w,Fe.value=Z<M-w}c&&(c.offsetWidth,c.classList.remove("transition-disabled"))}const Et=Ye(a=>{je(a.target)},64);Pa(at,{triggerRef:de(e,"trigger"),tabStyleRef:de(e,"tabStyle"),tabClassRef:de(e,"tabClass"),addTabStyleRef:de(e,"addTabStyle"),addTabClassRef:de(e,"addTabClass"),paneClassRef:de(e,"paneClass"),paneStyleRef:de(e,"paneStyle"),mergedClsPrefixRef:d,typeRef:de(e,"type"),closableRef:de(e,"closable"),valueRef:k,tabChangeIdRef:m,onBeforeLeaveRef:de(e,"onBeforeLeave"),activateTab:Be,handleClose:Oe,handleAdd:Bt}),La(()=>{N(),ye()}),Sa(()=>{const{value:a}=L;if(!a)return;const{value:s}=d,c=`${s}-tabs-nav-scroll-wrapper--shadow-start`,w=`${s}-tabs-nav-scroll-wrapper--shadow-end`;Y.value?a.classList.remove(c):a.classList.add(c),F.value?a.classList.remove(w):a.classList.add(w)});const Ot={syncBarPosition:()=>{N()},scrollToCurrentTab:()=>{ye()}},Lt=()=>{l({transitionDisabled:!0})},nt=ce(()=>{const{value:a}=ie,{type:s}=e,c=`${a}${{card:"Card",bar:"Bar",line:"Line",segment:"Segment"}[s]}`,{self:{barColor:w,closeIconColor:H,closeIconColorHover:J,closeIconColorPressed:M,tabColor:Z,tabBorderColor:fe,paneTextColor:It,tabFontWeight:At,tabBorderRadius:Wt,tabFontWeightActive:jt,colorSegment:Nt,fontWeightStrong:Ut,tabColorSegment:Dt,closeSize:Ht,closeIconSize:Vt,closeColorHover:Mt,closeColorPressed:Ft,closeBorderRadius:Jt,[se("panePadding",a)]:Ne,[se("tabPadding",c)]:qt,[se("tabPaddingVertical",c)]:Kt,[se("tabGap",c)]:Gt,[se("tabGap",`${c}Vertical`)]:Xt,[se("tabTextColor",s)]:Yt,[se("tabTextColorActive",s)]:Zt,[se("tabTextColorHover",s)]:Qt,[se("tabTextColorDisabled",s)]:ea,[se("tabFontSize",a)]:ta},common:{cubicBezierEaseInOut:aa}}=v.value;return{"--n-bezier":aa,"--n-color-segment":Nt,"--n-bar-color":w,"--n-tab-font-size":ta,"--n-tab-text-color":Yt,"--n-tab-text-color-active":Zt,"--n-tab-text-color-disabled":ea,"--n-tab-text-color-hover":Qt,"--n-pane-text-color":It,"--n-tab-border-color":fe,"--n-tab-border-radius":Wt,"--n-close-size":Ht,"--n-close-icon-size":Vt,"--n-close-color-hover":Mt,"--n-close-color-pressed":Ft,"--n-close-border-radius":Jt,"--n-close-icon-color":H,"--n-close-icon-color-hover":J,"--n-close-icon-color-pressed":M,"--n-tab-color":Z,"--n-tab-font-weight":At,"--n-tab-font-weight-active":jt,"--n-tab-padding":qt,"--n-tab-padding-vertical":Kt,"--n-tab-gap":Gt,"--n-tab-gap-vertical":Xt,"--n-pane-padding-left":Ue(Ne,"left"),"--n-pane-padding-right":Ue(Ne,"right"),"--n-pane-padding-top":Ue(Ne,"top"),"--n-pane-padding-bottom":Ue(Ne,"bottom"),"--n-font-weight-strong":Ut,"--n-tab-color-segment":Dt}}),ot=x?St("tabs",ce(()=>`${ie.value[0]}${e.type[0]}`),nt,e):void 0;return{mergedClsPrefix:d,mergedValue:k,renderedNames:new Set,segmentCapsuleElRef:t,tabsPaneWrapperRef:ne,tabsElRef:y,selfElRef:P,barElRef:W,addTabInstRef:U,xScrollInstRef:$,scrollWrapperElRef:L,addTabFixed:Te,tabWrapperStyle:V,handleNavResize:Ce,mergedSize:ie,handleScroll:Et,handleTabsResize:Pt,cssVars:x?void 0:nt,themeClass:ot?.themeClass,animationDirection:Re,renderNameListRef:_e,yScrollElRef:I,handleSegmentResize:Lt,onAnimationBeforeLeave:xe,onAnimationEnter:be,onAnimationAfterEnter:Pe,onRender:ot?.onRender,startReachedRef:Y,endReachedRef:F,isOverflow:Fe,handleButtonClick:Le,mergedTheme:v,rtlEnabled:C,mergedPlacement:g,...Ot}},render(){const{mergedClsPrefix:e,type:n,mergedPlacement:d,addTabFixed:x,addable:h,mergedSize:f,renderNameListRef:C,onRender:g,paneWrapperClass:v,paneWrapperStyle:y,startReachedRef:P,endReachedRef:W,isOverflow:L,showScrollButton:U,handleButtonClick:$,mergedTheme:I,rtlEnabled:Y,$slots:{default:F,prefix:j,suffix:ie}}=this;g?.();const te=F?Je(F()).filter(z=>z.type.__TAB_PANE__===!0):[],ee=F?Je(F()).filter(z=>z.type.__TAB__===!0):[],k=!ee.length,m=n==="card",V=n==="segment",B=!m&&!V&&this.justifyContent;C.value=[];const K=()=>{const z=(i(),p("div",{style:me(this.tabWrapperStyle),class:E(`${e}-tabs-wrapper`)},[B?T(()=>null):(i(),p("div",{key:1,class:E(`${e}-tabs-scroll-padding`),style:me(d==="top"||d==="bottom"?{width:`${this.tabsPadding}px`}:{height:`${this.tabsPadding}px`})},null,6)),k?(i(),p(Q,{key:2},[T(()=>te.map((N,ue)=>(C.value.push(N.props.name),Ze((i(),O(et,Ve(N.props,{internalCreatedByPane:!0,internalLeftPadded:ue!==0&&(!B||B==="center"||B==="start"||B==="end")}),st(N.children?{default:N.children.tab}:void 0),1040,["internalLeftPadded"]))))))],64)):(i(),p(Q,{key:3},[T(()=>ee.map((N,ue)=>(C.value.push(N.props.name),Ze(ue!==0&&!B?xt(N):N))))],64)),!x&&h&&m?(i(),p(Q,{key:4},[T(()=>yt(h,(k?te.length:ee.length)!==0))],64)):T(()=>null),B?T(()=>null):(i(),p("div",{key:7,class:E(`${e}-tabs-scroll-padding`),style:me({width:`${this.tabsPadding}px`})},null,6)),m?T(()=>null):(i(),p("div",{key:9,ref:"barElRef",class:E(`${e}-tabs-bar`)},null,2))],6));return i(),p("div",{ref:"tabsElRef",class:E(`${e}-tabs-nav-scroll-content`)},[m&&h?(i(),O(qe,{key:0,onResize:this.handleTabsResize},{default:()=>z},1032,["onResize"])):(i(),p(Q,{key:1},[T(()=>z)],64)),m?(i(),p("div",{key:2,class:E(`${e}-tabs-pad`)},null,2)):T(()=>null)],2)},G=V?"top":d;return i(),p("div",{ref:"selfElRef",class:E([`${e}-tabs`,this.themeClass,`${e}-tabs--${n}-type`,`${e}-tabs--${f}-size`,B&&`${e}-tabs--flex`,`${e}-tabs--${G}`,Y&&`${e}-tabs--rtl`]),style:me(this.cssVars)},[b("div",{class:E([`${e}-tabs-nav--${n}-type`,`${e}-tabs-nav--${G}`,`${e}-tabs-nav`])},[T(()=>Qe(j,z=>z&&(i(),p("div",{class:E(`${e}-tabs-nav__prefix`)},[T(()=>z)],2)))),V?(i(),O(qe,{key:0,onResize:this.handleSegmentResize},{default:()=>(i(),p("div",{class:E(`${e}-tabs-rail`),ref:"tabsElRef"},[b("div",{class:E(`${e}-tabs-capsule`),ref:"segmentCapsuleElRef"},[b("div",{class:E(`${e}-tabs-wrapper`)},[b("div",{class:E(`${e}-tabs-tab`)},null,2)],2)],2),k?(i(),p(Q,{key:0},[T(()=>te.map((z,N)=>(C.value.push(z.props.name),i(),O(et,Ve(z.props,{internalCreatedByPane:!0,internalLeftPadded:N!==0}),st(z.children?{default:z.children.tab}:void 0),1040,["internalLeftPadded"]))))],64)):(i(),p(Q,{key:1},[T(()=>ee.map((z,N)=>(C.value.push(z.props.name),N===0?z:xt(z))))],64))],2))},1032,["onResize"])):(i(),p(Q,{key:1},[T(()=>U&&L&&(i(),O(gt,{mergedClsPrefix:e,type:"prev",vertical:G==="left"||G==="right",disabled:P,rtl:!!Y,theme:I.peers.Button,themeOverrides:I.peerOverrides.Button,onClick:$},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"]))),(i(),O(qe,{onResize:this.handleNavResize},{default:()=>(i(),p("div",{class:E(`${e}-tabs-nav-scroll-wrapper`),ref:"scrollWrapperElRef"},[["top","bottom"].includes(G)?(i(),O(Ya,{key:0,ref:"xScrollInstRef",onScroll:this.handleScroll},{default:K},1032,["onScroll"])):(i(),p("div",{key:1,class:E(`${e}-tabs-nav-y-scroll`),onScroll:this.handleScroll,ref:"yScrollElRef"},[T(()=>K())],42,["onScroll"]))],2))},1032,["onResize"])),T(()=>U&&L&&(i(),O(gt,{mergedClsPrefix:e,type:"next",vertical:G==="left"||G==="right",disabled:W,rtl:!!Y,theme:I.peers.Button,themeOverrides:I.peerOverrides.Button,onClick:$},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"])))],64)),x&&h&&m?(i(),p(Q,{key:2},[T(()=>yt(h,!0))],64)):T(()=>null),T(()=>Qe(ie,z=>z&&(i(),p("div",{class:E(`${e}-tabs-nav__suffix`)},[T(()=>z)],2))))],2),T(()=>k&&(this.animated&&(G==="top"||G==="bottom")?(i(),p("div",{key:1,ref:"tabsPaneWrapperRef",style:me(y),class:E([`${e}-tabs-pane-wrapper`,v])},[T(()=>mt(te,this.mergedValue,this.renderedNames,this.onAnimationBeforeLeave,this.onAnimationEnter,this.onAnimationAfterEnter,this.animationDirection))],6)):mt(te,this.mergedValue,this.renderedNames)))],6)}});function mt(e,n,d,x,h,f,C){const g=[];return e.forEach(v=>{const{name:y,displayDirective:P,"display-directive":W}=v.props,L=$=>P===$||W===$,U=n===y;if(v.key!==void 0&&(v.key=y),U||L("show")||L("show:lazy")&&d.has(y)){d.has(y)||d.add(y);const $=!L("if");g.push($?_a(v,[[za,U]]):v)}}),C?(i(),O(Ta,{name:`${C}-transition`,onBeforeLeave:x,onEnter:h,onAfterEnter:f},{default:()=>g},1032,["name","onBeforeLeave","onEnter","onAfterEnter"])):g}function yt(e,n){return i(),O(et,{ref:"addTabInstRef",key:"__addable",name:"__addable",internalCreatedByPane:!0,internalAddable:!0,internalLeftPadded:n,disabled:typeof e=="object"&&e.disabled},null,8,["internalLeftPadded","disabled"])}function xt(e){const n=$a(e);return n.props?n.props.internalLeftPadded=!0:n.props={internalLeftPadded:!0},n}function Ze(e){return Array.isArray(e.dynamicProps)?e.dynamicProps.includes("internalLeftPadded")||e.dynamicProps.push("internalLeftPadded"):e.dynamicProps=["internalLeftPadded"],e}const br={class:"page-view"},fr={class:"page-intro config-page-intro"},vr={class:"config-toolbar"},pr={class:"config-meta-row"},hr={key:0,class:"muted"},gr={key:0,class:"revision-panel"},mr={key:0,class:"muted"},yr={class:"config-editor-layout"},xr={class:"config-groups"},kr=["onClick"],wr={class:"config-editor"},Cr={class:"editor-view-tabs"},Sr={key:0,class:"field-list"},_r={class:"field-copy"},Rr={key:0},zr={class:"field-control"},Tr={class:"save-bar"},$r={class:"system-settings-panel"},Pr={class:"field-copy"},Br={class:"field-control"},Er={class:"save-bar"},jr=he({__name:"ConfigView",setup(e){const n=Ba(),d=la(),x=A("profile"),h=A("visual"),f=A(""),C=A("ai"),g=A(""),v=A(""),y=it({}),P=it({log_level:"info",request_timeout_seconds:300,artifact_quota_bytes:4*1024*1024*1024,artifact_stale_upload_seconds:86400,modal_fallback_enabled:!1,modal_fallback_provider_id:"",modal_fallback_vision_model:"",modal_fallback_audio_model:""}),W=A([]),L=A(!1),U=A("{}"),$=A(!1),I=A(!1),Y=A(null),F=[{key:"ai",label:"AI 与模型"},{key:"persona",label:"人格"},{key:"context",label:"上下文管理"},{key:"agent",label:"Agent 执行"},{key:"workspace",label:"项目能力"},{key:"message",label:"消息输出"},{key:"memory",label:"长期记忆"}],j=ce(()=>n.configProfiles.find(r=>r.id===f.value)||n.defaultProfile),ie=ce(()=>n.configProfiles.map(r=>({label:`${r.name}${r.is_default?" · 默认":""}`,value:r.id}))),te=ce(()=>(n.configSchema.fields||[]).filter(r=>!g.value.trim()&&r.group!==C.value||!ue(r)?!1:g.value.trim()?[r.label,r.key,r.help].filter(Boolean).join(" ").toLocaleLowerCase().includes(g.value.trim().toLocaleLowerCase()):!0));function ee(r){return JSON.parse(JSON.stringify(r??{}))}function k(r){return Object.prototype.hasOwnProperty.call(y,r.key)?y[r.key]:r.default}function m(r){const t=k(r);return typeof t=="string"?t:t==null?"":String(t)}function V(r){const t=k(r);return typeof t=="number"?t:Number(t||0)}function B(r){return Object.prototype.hasOwnProperty.call(P,r.key)?P[r.key]:r.default}function K(r){const t=B(r);return typeof t=="string"?t:t==null?"":String(t)}function G(r){const t=B(r);return typeof t=="number"?t:Number(t||0)}function z(r,t){P[r.key]=t}function N(){const r=n.systemSettings;Object.keys(P).forEach(t=>{delete P[t]}),Object.assign(P,{log_level:r.log_level,request_timeout_seconds:r.request_timeout_seconds,artifact_quota_bytes:r.artifact_quota_bytes,artifact_stale_upload_seconds:r.artifact_stale_upload_seconds,modal_fallback_enabled:r.modal_fallback_enabled,modal_fallback_provider_id:r.modal_fallback_provider_id,modal_fallback_vision_model:r.modal_fallback_vision_model,modal_fallback_audio_model:r.modal_fallback_audio_model})}function ue(r){return Object.entries(r.display_if||{}).every(([t,l])=>{const R=n.configSchema.fields.find(Ce=>Ce.key===t),we=R?k(R):y[t];return JSON.stringify(we)===JSON.stringify(l)})}function ye(r){return r.option_source==="providers"?[{label:"沿用供应商注册表默认",value:""},...n.providers.map(t=>({label:t.name,value:t.id}))]:(r.options||[]).map(t=>({label:t.label,value:t.value}))}function ne(r,t){y[r.key]=t,L.value=!0}function pe(r){Object.keys(y).forEach(t=>delete y[t]),Object.assign(y,ee(r?.values||{})),v.value=r?.name||"",U.value=JSON.stringify(y,null,2),L.value=!1}async function oe(r){L.value&&!window.confirm("当前配置有未保存修改，确认放弃吗？")||(f.value=r,pe(n.configProfiles.find(t=>t.id===r)),await xe())}async function xe(){if(W.value=[],!!f.value)try{W.value=await Ea(f.value)}catch(r){d.error(r instanceof Error?r.message:"读取修订历史失败")}}async function be(r=f.value){await n.loadConfig(),f.value=r||n.defaultProfileID||n.configProfiles[0]?.id||"",pe(n.configProfiles.find(t=>t.id===f.value)),N(),await xe()}async function Pe(){if(f.value){if(h.value==="json")try{const r=JSON.parse(U.value);Object.keys(y).forEach(t=>delete y[t]),Object.assign(y,r)}catch(r){d.error(`JSON 格式无效：${r instanceof Error?r.message:"无法解析"}`);return}$.value=!0;try{const r={id:f.value,name:v.value.trim(),values:y},t=await ve(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/validate`,{method:"POST",body:JSON.stringify(r)});if(!t.valid)throw new Error(t.error||"配置校验失败");await ve(`/api/v1/config-profiles/${encodeURIComponent(f.value)}`,{method:"PUT",body:JSON.stringify(r)}),await be(f.value),d.success("配置已保存并立即生效")}catch(r){d.error(r instanceof Error?r.message:"保存配置失败")}finally{$.value=!1}}}async function _e(){const r=window.prompt("请输入配置文件名称","新配置");if(r?.trim())try{const t=await ve("/api/v1/config-profiles",{method:"POST",body:JSON.stringify({name:r.trim(),values:{}})});await be(t.id),d.success("配置文件已创建")}catch(t){d.error(t instanceof Error?t.message:"创建配置文件失败")}}async function Re(){if(!f.value)return;const r=window.prompt("请输入副本名称",`${j.value?.name||"配置"} 副本`);if(r?.trim())try{const t=await ve(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/copy`,{method:"POST",body:JSON.stringify({name:r.trim()})});await be(t.id),d.success("配置副本已创建")}catch(t){d.error(t instanceof Error?t.message:"复制配置失败")}}async function Be(){if(f.value)try{await ve(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/default`,{method:"POST",body:"{}"}),await be(f.value),d.success("已切换系统默认配置")}catch(r){d.error(r instanceof Error?r.message:"切换默认配置失败")}}async function Ee(){if(!(!j.value||j.value.is_default)&&window.confirm(`彻底删除“${j.value.name}”及全部修订历史？`))try{await ve(`/api/v1/config-profiles/${encodeURIComponent(j.value.id)}`,{method:"DELETE"}),await be(),d.success("配置文件已删除")}catch(r){d.error(r instanceof Error?r.message:"删除配置失败")}}async function Oe(){if(f.value)try{const r=await ve(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/export`),t=document.createElement("a");t.href=URL.createObjectURL(new Blob([JSON.stringify(r,null,2)],{type:"application/json"})),t.download=`${f.value}.json`,t.click(),URL.revokeObjectURL(t.href)}catch(r){d.error(r instanceof Error?r.message:"导出配置失败")}}async function Le(r){const t=r.target,l=t.files?.[0];if(t.value="",!!l)try{const R=JSON.parse(await l.text()),we={version:R.version,id:R.id,name:R.name,values:R.values||{},overwrite:!1};let Ce;try{Ce=await ve("/api/v1/config-profiles/import",{method:"POST",body:JSON.stringify(we)})}catch(ze){if(!(ze instanceof Error)||!/已存在|冲突/.test(ze.message)||!window.confirm("配置文件已存在，确认覆盖同 ID 配置吗？"))throw ze;we.overwrite=!0,Ce=await ve("/api/v1/config-profiles/import",{method:"POST",body:JSON.stringify(we)})}await be(Ce.id),d.success("配置已导入")}catch(R){d.error(R instanceof Error?R.message:"导入配置失败")}}async function ke(){try{const r=await ve("/api/v1/system-settings",{method:"PUT",body:JSON.stringify(P)});Object.assign(n.systemSettings,r),N(),d.success("系统设置已保存并立即生效")}catch(r){d.error(r instanceof Error?r.message:"保存系统设置失败")}}return zt(async()=>{await n.loadAll(),f.value=n.defaultProfileID||n.configProfiles[0]?.id||"",pe(j.value),N(),await xe()}),(r,t)=>(i(),p("div",br,[b("div",fr,[t[12]||(t[12]=b("div",null,[b("p",{class:"eyebrow"},"CONFIG CENTER"),b("h2",null,"配置文件"),b("p",null,"普通配置和系统设置共用同一套 Schema 表单；每次保存都会先校验，成功后立即应用到下一轮请求。")],-1)),q(_(lt),null,{default:X(()=>[q(_(le),{secondary:"",onClick:Oe},{default:X(()=>[...t[10]||(t[10]=[ae("导出配置",-1)])]),_:1}),q(_(le),{secondary:"",onClick:t[0]||(t[0]=l=>Y.value?.click())},{default:X(()=>[...t[11]||(t[11]=[ae("导入配置",-1)])]),_:1}),b("input",{ref_key:"importInput",ref:Y,type:"file",accept:"application/json,.json",class:"visually-hidden",onChange:Le},null,544)]),_:1})]),q(_(sa),{class:"config-card",bordered:!1},{default:X(()=>[q(_(ur),{value:x.value,"onUpdate:value":t[9]||(t[9]=l=>x.value=l),type:"segment",animated:""},{default:X(()=>[q(_(ht),{name:"profile",tab:"普通配置"},{default:X(()=>[b("div",vr,[q(_(Ge),{class:"profile-select",value:f.value,options:ie.value,"onUpdate:value":oe},null,8,["value","options"]),q(_($e),{value:g.value,"onUpdate:value":t[1]||(t[1]=l=>g.value=l),clearable:"",placeholder:"搜索设置名称、说明或配置键",class:"config-search"},null,8,["value"]),q(_(lt),null,{default:X(()=>[q(_(le),{secondary:"",onClick:_e},{default:X(()=>[...t[13]||(t[13]=[ae("新建",-1)])]),_:1}),q(_(le),{secondary:"",disabled:!j.value,onClick:Re},{default:X(()=>[...t[14]||(t[14]=[ae("复制",-1)])]),_:1},8,["disabled"]),q(_(le),{secondary:"",disabled:!j.value||j.value.is_default,onClick:Be},{default:X(()=>[...t[15]||(t[15]=[ae("设为默认",-1)])]),_:1},8,["disabled"]),q(_(le),{tertiary:"",type:"error",disabled:!j.value||j.value.is_default,onClick:Ee},{default:X(()=>[...t[16]||(t[16]=[ae("删除",-1)])]),_:1},8,["disabled"])]),_:1})]),b("div",pr,[b("div",null,[q(_($e),{value:v.value,"onUpdate:value":[t[2]||(t[2]=l=>v.value=l),t[3]||(t[3]=l=>L.value=!0)],placeholder:"配置名称"},null,8,["value"]),j.value?(i(),p("span",hr,"当前修订 v"+re(j.value.revision)+re(j.value.is_default?" · 系统默认":""),1)):ge("",!0)]),q(_(le),{text:"",onClick:t[4]||(t[4]=l=>I.value=!I.value)},{default:X(()=>[ae(re(I.value?"收起修订历史":"查看修订历史"),1)]),_:1})]),I.value?(i(),p("div",gr,[(i(!0),p(Q,null,De(W.value,l=>(i(),p("div",{key:`${l.profile_id}-${l.revision}`,class:"revision-row"},[b("strong",null,"v"+re(l.revision),1),b("span",null,re(l.created_at?new Date(l.created_at).toLocaleString():"未知时间"),1),l.revision===j.value?.revision?(i(),O(_(Ke),{key:0,size:"small",type:"success"},{default:X(()=>[...t[17]||(t[17]=[ae("当前",-1)])]),_:1})):ge("",!0)]))),128)),W.value.length?ge("",!0):(i(),p("span",mr,"暂无修订历史"))])):ge("",!0),b("div",yr,[b("aside",xr,[(i(),p(Q,null,De(F,l=>b("button",{key:l.key,type:"button",class:dt({active:C.value===l.key}),onClick:R=>C.value=l.key},re(l.label),11,kr)),64))]),b("section",wr,[b("div",Cr,[q(_(le),{size:"small",type:h.value==="visual"?"primary":"default",onClick:t[5]||(t[5]=l=>h.value="visual")},{default:X(()=>[...t[18]||(t[18]=[ae("可视化",-1)])]),_:1},8,["type"]),q(_(le),{size:"small",type:h.value==="json"?"primary":"default",onClick:t[6]||(t[6]=l=>{U.value=JSON.stringify(y,null,2),h.value="json"})},{default:X(()=>[...t[19]||(t[19]=[ae("JSON",-1)])]),_:1},8,["type"])]),h.value==="visual"?(i(),p("div",Sr,[(i(!0),p(Q,null,De(te.value,l=>(i(),p("div",{key:l.key,class:"config-field-row"},[b("div",_r,[b("strong",null,re(l.label),1),b("code",null,re(l.key),1),l.help?(i(),p("span",Rr,re(l.help),1)):ge("",!0),l.restart_required?(i(),O(_(Ke),{key:1,size:"small",type:"warning"},{default:X(()=>[...t[20]||(t[20]=[ae("需重启",-1)])]),_:1})):ge("",!0)]),b("div",zr,[l.type==="boolean"?(i(),O(_(pt),{key:0,checked:k(l)===!0,"onUpdate:checked":R=>ne(l,R)},null,8,["checked","onUpdate:checked"])):l.type==="select"?(i(),O(_(Ge),{key:1,value:String(k(l)??""),options:ye(l),"onUpdate:value":R=>ne(l,R)},null,8,["value","options","onUpdate:value"])):l.type==="integer"||l.type==="number"?(i(),O(_(bt),{key:2,value:V(l),min:l.min,max:l.max,step:l.type==="number"?.01:1,"show-button":!1,"onUpdate:value":R=>ne(l,R)},null,8,["value","min","max","step","onUpdate:value"])):l.type==="textarea"?(i(),O(_($e),{key:3,type:"textarea",value:m(l),autosize:{minRows:3,maxRows:8},"onUpdate:value":R=>ne(l,R)},null,8,["value","onUpdate:value"])):(i(),O(_($e),{key:4,value:m(l),type:l.secret?"password":"text","onUpdate:value":R=>ne(l,R)},null,8,["value","type","onUpdate:value"]))])]))),128)),te.value.length?ge("",!0):(i(),O(_(ra),{key:0,description:"当前没有匹配的设置"}))])):(i(),O(_($e),{key:1,value:U.value,"onUpdate:value":[t[7]||(t[7]=l=>U.value=l),t[8]||(t[8]=l=>L.value=!0)],type:"textarea",autosize:{minRows:18,maxRows:30},class:"json-editor"},null,8,["value"])),b("div",Tr,[b("span",{class:dt({dirty:L.value})},re(L.value?"有未保存修改":"配置已同步"),3),q(_(le),{type:"primary",loading:$.value,disabled:!j.value,onClick:Pe},{default:X(()=>[...t[21]||(t[21]=[ae("保存当前修订",-1)])]),_:1},8,["loading","disabled"])])])])]),_:1}),q(_(ht),{name:"system",tab:"系统设置"},{default:X(()=>[b("div",$r,[t[25]||(t[25]=b("div",{class:"section-heading-row"},[b("div",null,[b("h3",null,"系统设置"),b("p",null,"日志等级、全局请求超时、Artifact 维护和多模态降级保存后立即生效。")])],-1)),(i(!0),p(Q,null,De(_(n).systemSettings.schema||[],l=>(i(),p("div",{key:l.key,class:"config-field-row"},[b("div",Pr,[b("strong",null,re(l.label),1),b("code",null,"system."+re(l.key),1),b("span",null,re(l.help),1),l.restart_required?(i(),O(_(Ke),{key:0,size:"small",type:"warning"},{default:X(()=>[...t[22]||(t[22]=[ae("需重启",-1)])]),_:1})):ge("",!0)]),b("div",Br,[l.type==="boolean"?(i(),O(_(pt),{key:0,checked:B(l)===!0,"onUpdate:checked":R=>z(l,R)},null,8,["checked","onUpdate:checked"])):l.type==="select"?(i(),O(_(Ge),{key:1,value:String(B(l)??""),options:l.options||[],"onUpdate:value":R=>z(l,R)},null,8,["value","options","onUpdate:value"])):l.type==="integer"||l.type==="number"?(i(),O(_(bt),{key:2,value:G(l),min:l.min,max:l.max,step:l.type==="number"?.01:1,"show-button":!1,"onUpdate:value":R=>z(l,R)},null,8,["value","min","max","step","onUpdate:value"])):(i(),O(_($e),{key:3,value:K(l),type:l.secret?"password":"text","onUpdate:value":R=>z(l,R)},null,8,["value","type","onUpdate:value"]))])]))),128)),b("div",Er,[t[24]||(t[24]=b("span",null,"保存后立即应用",-1)),q(_(le),{type:"primary",onClick:ke},{default:X(()=>[...t[23]||(t[23]=[ae("保存系统设置",-1)])]),_:1})])])]),_:1})]),_:1},8,["value"])]),_:1})]))}});export{jr as default};
