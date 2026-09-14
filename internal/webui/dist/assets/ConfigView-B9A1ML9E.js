import{I as Pe,E as aa}from"./index-DxKR-8Sz.js";import{s as ra,v as je,x as na,d as xe,y as oa,z as la,r as O,A as Ue,a as b,C as S,D as n,E as u,G as U,H as sa,I as ia,J as da,K as De,L as Xe,h as s,c as p,M as z,N as P,b as K,O as ca,P as ua,Q as ye,U as Ze,V as mt,W as yt,X as ba,Y as fa,Z as xt,_ as kt,p as ve,$ as va,a0 as pa,a1 as we,a2 as de,a3 as fe,a4 as wt,a5 as ha,a6 as Ne,F as Q,f as I,a7 as ga,a8 as ma,a9 as ya,aa as Ct,ab as xa,B as ie,ac as ka,ad as Me,ae as Fe,k as Be,o as St,af as wa,ag as Ca,ah as Sa,ai as nt,aj as Ee,ak as We,al as Ra,am as _a,an as za,ao as Ta,j as $a,u as Pa,w as X,e as _,S as ot,l as lt,ap as Ba,i as ae,t as re,n as me,m as Ae,T as Ve,q as st,g as he}from"./index-ug_eSY-g.js";import{C as Ea}from"./Card-50kdVrGB.js";import{c as Oa,a as it,o as La,u as dt,S as Je}from"./Select-BDHb88T4.js";import{A as Ia,I as ct}from"./InputNumber-CPadmxje.js";import"./keysOf-HiGXOwLp.js";var Wa=/\s/;function Aa(e){for(var r=e.length;r--&&Wa.test(e.charAt(r)););return r}var ja=/^\s+/;function Na(e){return e&&e.slice(0,Aa(e)+1).replace(ja,"")}var ut=NaN,Ua=/^[-+]0x[0-9a-f]+$/i,Da=/^0b[01]+$/i,Ha=/^0o[0-7]+$/i,Ma=parseInt;function bt(e){if(typeof e=="number")return e;if(ra(e))return ut;if(je(e)){var r=typeof e.valueOf=="function"?e.valueOf():e;e=je(r)?r+"":r}if(typeof e!="string")return e===0?e:+e;e=Na(e);var i=Da.test(e);return i||Ha.test(e)?Ma(e.slice(2),i?2:8):Ua.test(e)?ut:+e}var qe=function(){return na.Date.now()},Fa="Expected a function",Va=Math.max,Ja=Math.min;function qa(e,r,i){var x,h,f,C,g,v,y=0,R=!1,W=!1,B=!0;if(typeof e!="function")throw new TypeError(Fa);r=bt(r)||0,je(i)&&(R=!!i.leading,W="maxWait"in i,f=W?Va(bt(i.maxWait)||0,r):f,B="trailing"in i?!!i.trailing:B);function N(k){var m=x,M=h;return x=h=void 0,y=k,C=e.apply(M,m),C}function T(k){return y=k,g=setTimeout(J,r),R?N(k):C}function E(k){var m=k-v,M=k-y,j=r-m;return W?Ja(j,f-M):j}function Y(k){var m=k-v,M=k-y;return v===void 0||m>=r||m<0||W&&M>=f}function J(){var k=qe();if(Y(k))return A(k);g=setTimeout(J,E(k))}function A(k){return g=void 0,B&&x?N(k):(x=h=void 0,C)}function ce(){g!==void 0&&clearTimeout(g),y=0,x=v=h=g=void 0}function te(){return g===void 0?C:A(qe())}function ee(){var k=qe(),m=Y(k);if(x=arguments,h=this,v=k,m){if(g===void 0)return T(v);if(W)return clearTimeout(g),g=setTimeout(J,r),N(v)}return g===void 0&&(g=setTimeout(J,r)),C}return ee.cancel=ce,ee.flush=te,ee}var Ka="Expected a function";function Ga(e,r,i){var x=!0,h=!0;if(typeof e!="function")throw new TypeError(Ka);return je(i)&&(x="leading"in i?!!i.leading:x,h="trailing"in i?!!i.trailing:h),qa(e,r,{leading:x,maxWait:r,trailing:h})}const Xa=it(".v-x-scroll",{overflow:"auto",scrollbarWidth:"none"},[it("&::-webkit-scrollbar",{width:0,height:0})]),Ya=xe({name:"XScroll",props:{disabled:Boolean,onScroll:Function},setup(){const e=O(null);function r(h){!(h.currentTarget.offsetWidth<h.currentTarget.scrollWidth)||h.deltaY===0||(h.currentTarget.scrollLeft+=h.deltaY+h.deltaX,h.preventDefault())}const i=la();return Xa.mount({id:"vueuc/x-scroll",head:!0,anchorMetaName:Oa,ssr:i}),Object.assign({selfRef:e,handleWheel:r},{scrollTo(...h){var f;(f=e.value)===null||f===void 0||f.scrollTo(...h)}})},render(){return oa("div",{ref:"selfRef",onScroll:this.onScroll,onWheel:this.disabled?void 0:this.handleWheel,class:"v-x-scroll"},this.$slots)}});var Za=xe({name:"ChevronLeft",render(){return(()=>{const e=Ue("dfe229c2639b2082");return e[0]||(e[0]=b("svg",{viewBox:"0 0 16 16",fill:"none",xmlns:"http://www.w3.org/2000/svg"},[b("path",{d:"M10.3536 3.14645C10.5488 3.34171 10.5488 3.65829 10.3536 3.85355L6.20711 8L10.3536 12.1464C10.5488 12.3417 10.5488 12.6583 10.3536 12.8536C10.1583 13.0488 9.84171 13.0488 9.64645 12.8536L5.14645 8.35355C4.95118 8.15829 4.95118 7.84171 5.14645 7.64645L9.64645 3.14645C9.84171 2.95118 10.1583 2.95118 10.3536 3.14645Z",fill:"currentColor"})],-1))})()}}),Qa=()=>(()=>{const e=Ue("75be776d8875fa17");return e[0]||(e[0]=b("svg",{viewBox:"0 0 64 64",class:"check-icon"},[b("path",{d:"M50.42,16.76L22.34,39.45l-8.1-11.46c-1.12-1.58-3.3-1.96-4.88-0.84c-1.58,1.12-1.95,3.3-0.84,4.88l10.26,14.51  c0.56,0.79,1.42,1.31,2.38,1.45c0.16,0.02,0.32,0.03,0.48,0.03c0.8,0,1.57-0.27,2.2-0.78l30.99-25.03c1.5-1.21,1.74-3.42,0.52-4.92  C54.13,15.78,51.93,15.55,50.42,16.76z"})],-1))})(),er=()=>(()=>{const e=Ue("c6eed899356c8404");return e[0]||(e[0]=b("svg",{viewBox:"0 0 100 100",class:"line-icon"},[b("path",{d:"M80.2,55.5H21.4c-2.8,0-5.1-2.5-5.1-5.5l0,0c0-3,2.3-5.5,5.1-5.5h58.7c2.8,0,5.1,2.5,5.1,5.5l0,0C85.2,53.1,82.9,55.5,80.2,55.5z"})],-1))})(),tr=S([n("checkbox",`
 font-size: var(--n-font-size);
 outline: none;
 cursor: pointer;
 display: inline-flex;
 flex-wrap: nowrap;
 align-items: flex-start;
 word-break: break-word;
 line-height: var(--n-size);
 --n-merged-color-table: var(--n-color-table);
 `,[u("show-label","line-height: var(--n-label-line-height);"),S("&:hover",[n("checkbox-box",[U("border","border: var(--n-border-checked);")])]),S("&:focus:not(:active)",[n("checkbox-box",[U("border",`
 border: var(--n-border-focus);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),u("inside-table",[n("checkbox-box",`
 background-color: var(--n-merged-color-table);
 `)]),u("checked",[n("checkbox-box",`
 background-color: var(--n-color-checked);
 `,[n("checkbox-icon",[S(".check-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),u("indeterminate",[n("checkbox-box",[n("checkbox-icon",[S(".check-icon",`
 opacity: 0;
 transform: scale(.5);
 `),S(".line-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),u("checked, indeterminate",[S("&:focus:not(:active)",[n("checkbox-box",[U("border",`
 border: var(--n-border-checked);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),n("checkbox-box",`
 background-color: var(--n-color-checked);
 border-left: 0;
 border-top: 0;
 `,[U("border",{border:"var(--n-border-checked)"})])]),u("disabled",{cursor:"not-allowed"},[u("checked",[n("checkbox-box",`
 background-color: var(--n-color-disabled-checked);
 `,[U("border",{border:"var(--n-border-disabled-checked)"}),n("checkbox-icon",[S(".check-icon, .line-icon",{fill:"var(--n-check-mark-color-disabled-checked)"})])])]),n("checkbox-box",`
 background-color: var(--n-color-disabled);
 `,[U("border",`
 border: var(--n-border-disabled);
 `),n("checkbox-icon",[S(".check-icon, .line-icon",`
 fill: var(--n-check-mark-color-disabled);
 `)])]),U("label",`
 color: var(--n-text-color-disabled);
 `)]),n("checkbox-box-wrapper",`
 position: relative;
 width: var(--n-size);
 flex-shrink: 0;
 flex-grow: 0;
 user-select: none;
 -webkit-user-select: none;
 `),n("checkbox-box",`
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
 `,[U("border",`
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
 `),n("checkbox-icon",`
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
 `),sa({left:"1px",top:"1px"})])]),U("label",`
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 user-select: none;
 -webkit-user-select: none;
 padding: var(--n-label-padding);
 font-weight: var(--n-label-font-weight);
 `,[S("&:empty",{display:"none"})])]),ia(n("checkbox",`
 --n-merged-color-table: var(--n-color-table-modal);
 `)),da(n("checkbox",`
 --n-merged-color-table: var(--n-color-table-popover);
 `))]);const ar=["id"],rr=["tabindex","aria-checked","aria-labelledby","onKeyup","onKeydown","onClick"],nr={...De.props,size:String,checked:{type:[Boolean,String,Number],default:void 0},defaultChecked:{type:[Boolean,String,Number],default:!1},value:[String,Number],disabled:{type:Boolean,default:void 0},indeterminate:Boolean,label:String,focusable:{type:Boolean,default:!0},checkedValue:{type:[Boolean,String,Number],default:!0},uncheckedValue:{type:[Boolean,String,Number],default:!1},"onUpdate:checked":[Function,Array],onUpdateChecked:[Function,Array],privateInsideTable:Boolean,onChange:[Function,Array]};var or=xe({name:"Checkbox",props:nr,setup(e){const r=Ze(lr,null),i=O(null),{mergedClsPrefixRef:x,inlineThemeDisabled:h,mergedRtlRef:f,mergedComponentPropsRef:C}=mt(e),g=O(e.defaultChecked),v=fe(e,"checked"),y=yt(v,g),R=ba(()=>{if(r){const m=r.valueSetRef.value;return m&&e.value!==void 0?m.has(e.value):!1}else return y.value===e.checkedValue}),W=fa(e,{mergedSize(m){const{size:M}=e;if(M!==void 0)return M;if(r){const{value:G}=r.mergedSizeRef;if(G!==void 0)return G}if(m){const{mergedSize:G}=m;if(G!==void 0)return G.value}const j=C?.value?.Checkbox?.size;return j||"medium"},mergedDisabled(m){const{disabled:M}=e;if(M!==void 0)return M;if(r){if(r.disabledRef.value)return!0;const{maxRef:{value:j},checkedCountRef:G}=r;if(j!==void 0&&G.value>=j&&!R.value)return!0;const{minRef:{value:F}}=r;if(F!==void 0&&G.value<=F&&R.value)return!0}return m?m.disabled.value:!1}}),{mergedDisabledRef:B,mergedSizeRef:N}=W,T=De("Checkbox","-checkbox",tr,pa,e,x);function E(m){if(r&&e.value!==void 0)r.toggleCheckbox(!R.value,e.value);else{const{onChange:M,"onUpdate:checked":j,onUpdateChecked:G}=e,{nTriggerFormInput:F,nTriggerFormChange:$}=W,D=R.value?e.uncheckedValue:e.checkedValue;j&&we(j,D,m),G&&we(G,D,m),M&&we(M,D,m),F(),$(),g.value=D}}function Y(m){B.value||E(m)}function J(m){if(!B.value)switch(m.key){case" ":case"Enter":E(m)}}function A(m){m.key===" "&&m.preventDefault()}const ce={focus:()=>{i.value?.focus()},blur:()=>{i.value?.blur()}},te=xt("Checkbox",f,x),ee=ve(()=>{const{value:m}=N,{common:{cubicBezierEaseInOut:M},self:{borderRadius:j,color:G,colorChecked:F,colorDisabled:$,colorTableHeader:D,colorTableHeaderModal:ne,colorTableHeaderPopover:oe,checkMarkColor:ge,checkMarkColorDisabled:ke,border:le,borderFocus:ze,borderDisabled:Te,borderChecked:$e,boxShadowFocus:Ce,textColor:Se,textColorDisabled:l,checkMarkColorDisabledChecked:t,colorDisabledChecked:d,borderDisabledChecked:L,labelPadding:ue,labelLineHeight:be,labelFontWeight:se,[de("fontSize",m)]:Re,[de("size",m)]:Oe}}=T.value;return{"--n-label-line-height":be,"--n-label-font-weight":se,"--n-size":Oe,"--n-bezier":M,"--n-border-radius":j,"--n-border":le,"--n-border-checked":$e,"--n-border-focus":ze,"--n-border-disabled":Te,"--n-border-disabled-checked":L,"--n-box-shadow-focus":Ce,"--n-color":G,"--n-color-checked":F,"--n-color-table":D,"--n-color-table-modal":ne,"--n-color-table-popover":oe,"--n-color-disabled":$,"--n-color-disabled-checked":d,"--n-text-color":Se,"--n-text-color-disabled":l,"--n-check-mark-color":ge,"--n-check-mark-color-disabled":ke,"--n-check-mark-color-disabled-checked":t,"--n-font-size":Re,"--n-label-padding":ue}}),k=h?kt("checkbox",ve(()=>N.value[0]),ee,e):void 0;return Object.assign(W,ce,{rtlEnabled:te,selfRef:i,mergedClsPrefix:x,mergedDisabled:B,renderedChecked:R,mergedTheme:T,labelId:va(),handleClick:Y,handleKeyUp:J,handleKeyDown:A,cssVars:h?void 0:ee,themeClass:k?.themeClass,onRender:k?.onRender})},render(){const{$slots:e,renderedChecked:r,mergedDisabled:i,indeterminate:x,privateInsideTable:h,cssVars:f,labelId:C,label:g,mergedClsPrefix:v,focusable:y,handleKeyUp:R,handleKeyDown:W,handleClick:B}=this;this.onRender?.();const N=Xe(e.default,T=>g||T?(s(),p("span",{key:1,class:P(`${v}-checkbox__label`),id:C},[z(()=>g||T)],10,ar)):null);return(()=>{const T=Ue("70be6e74cd27cb50");return s(),p("div",{ref:"selfRef",class:P([`${v}-checkbox`,this.themeClass,this.rtlEnabled&&`${v}-checkbox--rtl`,r&&`${v}-checkbox--checked`,i&&`${v}-checkbox--disabled`,x&&`${v}-checkbox--indeterminate`,h&&`${v}-checkbox--inside-table`,N&&`${v}-checkbox--show-label`]),tabindex:i||!y?void 0:0,role:"checkbox","aria-checked":x?"mixed":r,"aria-labelledby":C,style:ye(f),onKeyup:R,onKeydown:W,onClick:B,onMousedown:T[0]||(T[0]=()=>{ua("selectstart",window,E=>{E.preventDefault()},{once:!0})})},[b("div",{class:P(`${v}-checkbox-box-wrapper`)},[T[1]||(T[1]=z(" ",-1)),b("div",{class:P(`${v}-checkbox-box`)},[K(ca,null,{default:()=>this.indeterminate?(s(),p("div",{key:"indeterminate",class:P(`${v}-checkbox-icon`)},[z(()=>er())],2)):(s(),p("div",{key:"check",class:P(`${v}-checkbox-icon`)},[z(()=>Qa())],2))},1024),b("div",{class:P(`${v}-checkbox-box__border`)},null,2)],2)],2),z(()=>N)],46,rr)})()}});const lr=wt("n-checkbox-group"),Qe=wt("n-tabs"),Rt={tab:[String,Number,Object,Function],name:{type:[String,Number],required:!0},disabled:Boolean,displayDirective:{type:String,default:"if"},closable:{type:Boolean,default:void 0},tabProps:Object,label:[String,Number,Object,Function]};var ft=xe({__TAB_PANE__:!0,name:"TabPane",alias:["TabPanel"],props:Rt,slots:Object,setup(e){const r=Ze(Qe,null);return r||ha("tab-pane","`n-tab-pane` must be placed inside `n-tabs`."),{style:r.paneStyleRef,class:r.paneClassRef,mergedClsPrefix:r.mergedClsPrefixRef}},render(){return s(),p("div",{class:P([`${this.mergedClsPrefix}-tab-pane`,this.class]),style:ye(this.style)},[z(()=>this.$slots.default?.())],6)}});const sr=["data-name","data-disabled"],ir={internalLeftPadded:Boolean,internalAddable:Boolean,internalCreatedByPane:Boolean,...ya(Rt,["displayDirective"])};var Ye=xe({__TAB__:!0,inheritAttrs:!1,name:"Tab",props:ir,setup(e){const{mergedClsPrefixRef:r,valueRef:i,typeRef:x,closableRef:h,tabStyleRef:f,addTabStyleRef:C,tabClassRef:g,addTabClassRef:v,tabChangeIdRef:y,onBeforeLeaveRef:R,triggerRef:W,handleAdd:B,activateTab:N,handleClose:T}=Ze(Qe);return{trigger:W,mergedClosable:ve(()=>{if(e.internalAddable)return!1;const{closable:E}=e;return E===void 0?h.value:E}),style:f,addStyle:C,tabClass:g,addTabClass:v,clsPrefix:r,value:i,type:x,handleClose(E){E.stopPropagation(),!e.disabled&&T(e.name)},activateTab(){if(e.disabled)return;if(e.internalAddable){B();return}const{name:E}=e,Y=++y.id;if(E!==i.value){const{value:J}=R;J?Promise.resolve(J(e.name,i.value)).then(A=>{A&&y.id===Y&&N(E)}):N(E)}}}},render(){const{internalAddable:e,clsPrefix:r,name:i,disabled:x,label:h,tab:f,value:C,mergedClosable:g,trigger:v,$slots:{default:y}}=this,R=h??f;return s(),p("div",{class:P(`${r}-tabs-tab-wrapper`)},[this.internalLeftPadded?(s(),p("div",{key:0,class:P(`${r}-tabs-tab-pad`)},null,2)):z(()=>null),(s(),p("div",Ne({key:i,"data-name":i,"data-disabled":x?!0:void 0},Ne({class:[`${r}-tabs-tab`,C===i&&`${r}-tabs-tab--active`,x&&`${r}-tabs-tab--disabled`,g&&`${r}-tabs-tab--closable`,e&&`${r}-tabs-tab--addable`,e?this.addTabClass:this.tabClass],onClick:v==="click"?this.activateTab:void 0,onMouseenter:v==="hover"?this.activateTab:void 0,style:e?this.addStyle:this.style},this.internalCreatedByPane?this.tabProps||{}:this.$attrs)),[b("span",{class:P(`${r}-tabs-tab__label`)},[e?(s(),p(Q,{key:0},[b("div",{class:P(`${r}-tabs-tab__height-placeholder`)}," ",2),(s(),I(Ct,{clsPrefix:r},{default:()=>(s(),I(Ia))},1032,["clsPrefix"]))],64)):(s(),p(Q,{key:1},[y?(s(),p(Q,{key:0},[z(()=>y())],64)):(s(),p(Q,{key:1},[typeof R=="object"?(s(),p(Q,{key:0},[z(()=>R)],64)):(s(),p(Q,{key:1},[z(()=>ga(R??i))],64))],64))],64))],2),g&&this.type==="card"?(s(),I(ma,{key:0,clsPrefix:r,class:P(`${r}-tabs-tab__close`),onClick:this.handleClose,disabled:x},null,8,["clsPrefix","class","onClick","disabled"])):z(()=>null)],16,sr))],2)}}),dr=n("tabs",`
 box-sizing: border-box;
 width: 100%;
 display: flex;
 flex-direction: column;
 transition:
 background-color .3s var(--n-bezier),
 border-color .3s var(--n-bezier);
`,[S("&.transition-disabled",[n("tabs-tab",`
 transition: none !important;
 `),n("tabs-nav-scroll-content",`
 transition: none !important;
 `),n("tabs-tab-pad",`
 transition: none !important;
 `)]),u("segment-type",[n("tabs-rail",[S("&.transition-disabled",[n("tabs-capsule",`
 transition: none;
 `)])])]),u("top",[n("tab-pane",`
 padding: var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left);
 `)]),u("left",[n("tab-pane",`
 padding: var(--n-pane-padding-right) var(--n-pane-padding-bottom) var(--n-pane-padding-left) var(--n-pane-padding-top);
 `)]),u("left, right",`
 flex-direction: row;
 `,[n("tabs-bar",`
 width: 2px;
 right: 0;
 transition:
 top .2s var(--n-bezier),
 max-height .2s var(--n-bezier),
 background-color .3s var(--n-bezier);
 `),n("tabs-tab",`
 padding: var(--n-tab-padding-vertical); 
 `)]),u("right",`
 flex-direction: row-reverse;
 `,[n("tab-pane",`
 padding: var(--n-pane-padding-left) var(--n-pane-padding-top) var(--n-pane-padding-right) var(--n-pane-padding-bottom);
 `),n("tabs-bar",`
 left: 0;
 `)]),u("bottom",`
 flex-direction: column-reverse;
 justify-content: flex-end;
 `,[n("tab-pane",`
 padding: var(--n-pane-padding-bottom) var(--n-pane-padding-right) var(--n-pane-padding-top) var(--n-pane-padding-left);
 `),n("tabs-bar",`
 top: 0;
 `)]),n("tabs-rail",`
 position: relative;
 padding: 3px;
 border-radius: var(--n-tab-border-radius);
 width: 100%;
 background-color: var(--n-color-segment);
 transition: background-color .3s var(--n-bezier);
 display: flex;
 align-items: center;
 `,[n("tabs-capsule",`
 border-radius: var(--n-tab-border-radius);
 position: absolute;
 left: 0;
 top: 0;
 pointer-events: none;
 background-color: var(--n-tab-color-segment);
 box-shadow: 0 1px 3px 0 rgba(0, 0, 0, .08);
 transition: transform 0.3s var(--n-bezier);
 `),n("tabs-tab-wrapper",`
 flex-basis: 0;
 flex-grow: 1;
 display: flex;
 align-items: center;
 justify-content: center;
 `,[n("tabs-tab",`
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
 `)])])]),u("flex",[n("tabs-nav",`
 width: 100%;
 position: relative;
 `,[n("tabs-wrapper",`
 width: 100%;
 `,[n("tabs-tab",`
 margin-right: 0;
 `)])])]),n("tabs-nav",`
 box-sizing: border-box;
 line-height: 1.5;
 display: flex;
 transition: border-color .3s var(--n-bezier);
 `,[U("prefix, suffix",`
 display: flex;
 align-items: center;
 `),U("prefix","padding-right: 16px;"),U("suffix","padding-left: 16px;")]),u("top, bottom",[S(">",[n("tabs-nav",[n("tabs-nav-scroll-wrapper",[S("&::before",`
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
 `)])])])])]),u("left, right",[n("tabs-nav-scroll-content",`
 flex-direction: column;
 `),S(">",[n("tabs-nav",[n("tabs-nav-scroll-wrapper",[S("&::before",`
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
 `)])])])])]),n("tabs-nav-scroll-wrapper",`
 flex: 1;
 position: relative;
 overflow: hidden;
 `,[n("tabs-nav-y-scroll",`
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
 `)])]),n("tabs-nav-scroll-content",`
 display: flex;
 position: relative;
 min-width: 100%;
 min-height: 100%;
 width: fit-content;
 box-sizing: border-box;
 `),n("tabs-wrapper",`
 display: inline-flex;
 flex-wrap: nowrap;
 position: relative;
 `),n("tabs-tab-wrapper",`
 display: flex;
 flex-wrap: nowrap;
 flex-shrink: 0;
 flex-grow: 0;
 `),n("tabs-tab",`
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
 `,[u("disabled",{cursor:"not-allowed"}),U("close",`
 margin-inline-start: 6px;
 transition:
 background-color .3s var(--n-bezier),
 color .3s var(--n-bezier);
 `),U("label",`
 display: flex;
 align-items: center;
 z-index: 1;
 `)]),n("tabs-bar",`
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
 `)]),n("tabs-pane-wrapper",`
 position: relative;
 overflow: hidden;
 transition: max-height .2s var(--n-bezier);
 `),n("tab-pane",`
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
 `)]),n("tabs-tab-pad",`
 box-sizing: border-box;
 width: var(--n-tab-gap);
 flex-grow: 0;
 flex-shrink: 0;
 `),u("line-type, bar-type",[n("tabs-tab",`
 font-weight: var(--n-tab-font-weight);
 box-sizing: border-box;
 vertical-align: bottom;
 `,[S("&:hover",{color:"var(--n-tab-text-color-hover)"}),u("active",`
 color: var(--n-tab-text-color-active);
 font-weight: var(--n-tab-font-weight-active);
 `),u("disabled",{color:"var(--n-tab-text-color-disabled)"})])]),n("tabs-nav",[U("prefix, suffix",`
 border-color: var(--n-tab-border-color);
 `),n("tabs-nav-scroll-content",`
 border-color: var(--n-tab-border-color);
 `),u("line-type",[u("top",[U("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),n("tabs-nav-scroll-content",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),n("tabs-bar",`
 bottom: -1px;
 `)]),u("left",[U("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),n("tabs-nav-scroll-content",`
 border-right: 1px solid var(--n-tab-border-color);
 `),n("tabs-bar",`
 right: -1px;
 `)]),u("right",[U("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),n("tabs-nav-scroll-content",`
 border-left: 1px solid var(--n-tab-border-color);
 `),n("tabs-bar",`
 left: -1px;
 `)]),u("bottom",[U("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),n("tabs-nav-scroll-content",`
 border-top: 1px solid var(--n-tab-border-color);
 `),n("tabs-bar",`
 top: -1px;
 `)]),U("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),n("tabs-nav-scroll-content",`
 transition: border-color .3s var(--n-bezier);
 `),n("tabs-bar",`
 border-radius: 0;
 `)]),u("card-type",[U("prefix, suffix",`
 transition: border-color .3s var(--n-bezier);
 `),n("tabs-pad",`
 flex-grow: 1;
 transition: border-color .3s var(--n-bezier);
 `),n("tabs-tab-pad",`
 transition: border-color .3s var(--n-bezier);
 `),n("tabs-tab",`
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
 `,[U("height-placeholder",`
 width: 0;
 font-size: var(--n-tab-font-size);
 `),xa("disabled",[S("&:hover",`
 color: var(--n-tab-text-color-hover);
 `)])]),u("closable","padding-inline-end: 8px;"),u("active",`
 background-color: #0000;
 font-weight: var(--n-tab-font-weight-active);
 color: var(--n-tab-text-color-active);
 `),u("disabled","color: var(--n-tab-text-color-disabled);")])]),u("left, right",`
 flex-direction: column; 
 `,[U("prefix, suffix",`
 padding: var(--n-tab-padding-vertical);
 `),n("tabs-wrapper",`
 flex-direction: column;
 `),n("tabs-tab-wrapper",`
 flex-direction: column;
 `,[n("tabs-tab-pad",`
 height: var(--n-tab-gap-vertical);
 width: 100%;
 `)])]),u("top",[u("card-type",[n("tabs-scroll-padding","border-bottom: 1px solid var(--n-tab-border-color);"),U("prefix, suffix",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),n("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-top-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-bottom: 1px solid #0000;
 `)]),n("tabs-tab-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `),n("tabs-pad",`
 border-bottom: 1px solid var(--n-tab-border-color);
 `)])]),u("left",[u("card-type",[n("tabs-scroll-padding","border-right: 1px solid var(--n-tab-border-color);"),U("prefix, suffix",`
 border-right: 1px solid var(--n-tab-border-color);
 `),n("tabs-tab",`
 border-top-left-radius: var(--n-tab-border-radius);
 border-bottom-left-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-right: 1px solid #0000;
 `)]),n("tabs-tab-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `),n("tabs-pad",`
 border-right: 1px solid var(--n-tab-border-color);
 `)])]),u("right",[u("card-type",[n("tabs-scroll-padding","border-left: 1px solid var(--n-tab-border-color);"),U("prefix, suffix",`
 border-left: 1px solid var(--n-tab-border-color);
 `),n("tabs-tab",`
 border-top-right-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-left: 1px solid #0000;
 `)]),n("tabs-tab-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `),n("tabs-pad",`
 border-left: 1px solid var(--n-tab-border-color);
 `)])]),u("bottom",[u("card-type",[n("tabs-scroll-padding","border-top: 1px solid var(--n-tab-border-color);"),U("prefix, suffix",`
 border-top: 1px solid var(--n-tab-border-color);
 `),n("tabs-tab",`
 border-bottom-left-radius: var(--n-tab-border-radius);
 border-bottom-right-radius: var(--n-tab-border-radius);
 `,[u("active",`
 border-top: 1px solid #0000;
 `)]),n("tabs-tab-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `),n("tabs-pad",`
 border-top: 1px solid var(--n-tab-border-color);
 `)])])]),n("tabs-scroll-button",[u("start",`
 padding-left: 10px;
 padding-right: 6px;
 `),u("end",`
 padding-right: 10px;
 padding-left: 6px;
 `),u("up",`
 padding-bottom: 10px;
 `),u("down",`
 padding-top: 10px;
 `)])]),vt=xe({name:"TabsButton",props:{type:{type:String,default:"next"},mergedClsPrefix:{type:String,required:!0},vertical:Boolean,disabled:Boolean,rtl:Boolean,theme:Object,themeOverrides:Object,onClick:Function},setup(e){return{handleClick:()=>{e.disabled||e.onClick?.(e.type)}}},render(){const{mergedClsPrefix:e,disabled:r,type:i,vertical:x,rtl:h,theme:f,themeOverrides:C,handleClick:g}=this,v=i==="next",y=x?v:h?!v:v;return s(),I(ie,{text:!0,disabled:r,size:"small",theme:f,themeOverrides:C,onClick:g,class:P([`${e}-tabs-scroll-button`,!x&&i==="prev"&&`${e}-tabs-scroll-button--start`,!x&&i==="next"&&`${e}-tabs-scroll-button--end`,x&&i==="prev"&&`${e}-tabs-scroll-button--up`,x&&i==="next"&&`${e}-tabs-scroll-button--down`])},{icon:()=>(s(),I(Ct,{clsPrefix:e,style:ye(x?{transform:"rotate(90deg)"}:void 0)},{default:()=>y?(s(),I(ka,{key:1})):(s(),I(Za,{key:2}))},1032,["clsPrefix","style"]))},1032,["disabled","theme","themeOverrides","onClick","class"])}});const Ke=Ga,cr={...De.props,value:[String,Number],defaultValue:[String,Number],trigger:{type:String,default:"click"},type:{type:String,default:"bar"},closable:Boolean,justifyContent:String,size:String,placement:{type:String,default:"top"},tabStyle:[String,Object],tabClass:String,addTabStyle:[String,Object],addTabClass:String,barWidth:Number,paneClass:String,paneStyle:[String,Object],paneWrapperClass:String,paneWrapperStyle:[String,Object],addable:[Boolean,Object],tabsPadding:{type:Number,default:0},animated:Boolean,onBeforeLeave:Function,onAdd:Function,"onUpdate:value":[Function,Array],onUpdateValue:[Function,Array],onClose:[Function,Array],labelSize:String,activeName:[String,Number],onActiveNameChange:[Function,Array],showScrollButton:Boolean,centerActiveTab:Boolean};var ur=xe({name:"Tabs",props:cr,slots:Object,setup(e,{slots:r}){const{mergedClsPrefixRef:i,inlineThemeDisabled:x,mergedComponentPropsRef:h,mergedRtlRef:f}=mt(e),C=xt("Tabs",f,i),g=ve(()=>{const{placement:a}=e;return a==="start"?C?.value?"right":"left":a==="end"?C?.value?"left":"right":a}),v=De("Tabs","-tabs",dr,Sa,e,i),y=O(null),R=O(null),W=O(null),B=O(null),N=O(null),T=O(null),E=O(null),Y=O(!0),J=O(!0),A=dt(e,["labelSize","size"]),ce=ve(()=>{if(A.value)return A.value;const a=h?.value?.Tabs?.size;return a||"medium"}),te=dt(e,["activeName","value"]),ee=O(te.value??e.defaultValue??(r.default?Me(r.default())[0]?.props?.name:null)),k=yt(te,ee),m={id:0},M=ve(()=>{if(!(!e.justifyContent||e.type==="card"))return{display:"flex",justifyContent:e.justifyContent}});Be(k,()=>{m.id=0,D(),Ee(()=>{oe()})});function j(){const{value:a}=k;return a===null?null:y.value?.querySelector(`[data-name="${a}"]`)}function G(a){if(e.type==="card")return;const{value:o}=W;if(!o)return;const c=o.style.opacity==="0";if(a){const w=`${i.value}-tabs-bar--disabled`,{barWidth:H}=e,q=g.value;if(a.dataset.disabled==="true"?o.classList.add(w):o.classList.remove(w),["top","bottom"].includes(q)){if($(["top","maxHeight","height"]),typeof H=="number"&&a.offsetWidth>=H){const V=Math.floor((a.offsetWidth-H)/2)+a.offsetLeft;o.style.left=`${V}px`,o.style.maxWidth=`${H}px`}else o.style.left=`${a.offsetLeft}px`,o.style.maxWidth=`${a.offsetWidth}px`;o.style.width="8192px",c&&(o.style.transition="none"),o.offsetWidth,c&&(o.style.transition="",o.style.opacity="1")}else{if($(["left","maxWidth","width"]),typeof H=="number"&&a.offsetHeight>=H){const V=Math.floor((a.offsetHeight-H)/2)+a.offsetTop;o.style.top=`${V}px`,o.style.maxHeight=`${H}px`}else o.style.top=`${a.offsetTop}px`,o.style.maxHeight=`${a.offsetHeight}px`;o.style.height="8192px",c&&(o.style.transition="none"),o.offsetHeight,c&&(o.style.transition="",o.style.opacity="1")}}}function F(){if(e.type==="card")return;const{value:a}=W;a&&(a.style.opacity="0")}function $(a){const{value:o}=W;if(o)for(const c of a)o.style[c]=""}function D(){if(e.type==="card")return;const a=j();a?G(a):F()}function ne(a,o,c,w){const H=a.getBoundingClientRect(),q=o.getBoundingClientRect(),V=c?"left":"top",Z=c?"right":"bottom";let pe=0;w?pe=(q[V]+q[Z])/2-(H[V]+H[Z])/2:q[V]<H[V]?pe=q[V]-H[V]:q[Z]>H[Z]&&(pe=q[Z]-H[Z]),pe!==0&&a.scrollBy({[V]:pe,behavior:"smooth"})}function oe(){const a=["top","bottom"].includes(g.value),o=j();if(o)if(a){const c=T.value?.$el;if(!c)return;ne(c,o,a,e.centerActiveTab)}else{const{value:c}=E;if(!c)return;ne(c,o,a,e.centerActiveTab)}}const ge=O(null);let ke=0,le=null;function ze(a){const o=ge.value;if(o){ke=a.getBoundingClientRect().height;const c=`${ke}px`,w=()=>{o.style.height=c,o.style.maxHeight=c};le?(w(),le(),le=null):le=w}}function Te(a){const o=ge.value;if(o){const c=a.getBoundingClientRect().height,w=()=>{document.body.offsetHeight,o.style.maxHeight=`${c}px`,o.style.height=`${Math.max(ke,c)}px`};le?(le(),le=null,w()):le=w}}function $e(){const a=ge.value;if(a){a.style.maxHeight="",a.style.height="";const{paneWrapperStyle:o}=e;if(typeof o=="string")a.style.cssText=o;else if(o){const{maxHeight:c,height:w}=o;c!==void 0&&(a.style.maxHeight=c),w!==void 0&&(a.style.height=w)}}}const Ce={value:[]},Se=O("next");function l(a){const o=k.value;let c="next";for(const w of Ce.value){if(w===o)break;if(w===a){c="prev";break}}Se.value=c,t(a)}function t(a){const{onActiveNameChange:o,onUpdateValue:c,"onUpdate:value":w}=e;o&&we(o,a),c&&we(c,a),w&&we(w,a),ee.value=a}function d(a){const{onClose:o}=e;o&&we(o,a)}function L(a){if(["top","bottom"].includes(g.value)){const{value:o}=T;if(!o)return;const c=o.$el;if(!c)return;const w=c.offsetWidth,H=!!C?.value,q=a==="next"?w:-w;c.scrollBy({left:H?-q:q,behavior:"smooth"})}else{const{value:o}=E;if(!o)return;const c=o.offsetHeight,w=a==="next"?o.scrollTop+c:o.scrollTop-c;o.scrollTo({top:w,left:0,behavior:"smooth"})}}let ue=!0;function be(){const{value:a}=W;if(!a)return;ue&&(ue=!1);const o="transition-disabled";a.classList.add(o),D(),a.classList.remove(o)}const se=O(null);function Re({transitionDisabled:a}){const o=y.value;if(!o)return;a&&o.classList.add("transition-disabled");const c=j();c&&se.value&&(se.value.style.width=`${c.offsetWidth}px`,se.value.style.height=`${c.offsetHeight}px`,se.value.style.transform=`translate(${c.offsetLeft}px, ${c.offsetTop}px)`,a&&se.value.offsetWidth),a&&o.classList.remove("transition-disabled")}Be([k],()=>{e.type==="segment"&&Ee(()=>{Re({transitionDisabled:!1})})}),St(()=>{e.type==="segment"&&Re({transitionDisabled:!0})});let Oe=0;function _t(a){if(a.contentRect.width===0&&a.contentRect.height===0||Oe===a.contentRect.width)return;Oe=a.contentRect.width;const{type:o}=e;(o==="line"||o==="bar")&&(ue||e.justifyContent?.startsWith("space"))&&be(),o!=="segment"&&Le(tt())}const zt=Ke(_t,64);function et(){const{type:a}=e;a==="line"||a==="bar"?be():a==="segment"&&Re({transitionDisabled:!0})}Be([()=>e.justifyContent,()=>e.size],()=>{Ee(()=>{(e.type==="line"||e.type==="bar")&&be()})}),Be([g,()=>C?.value],()=>{Ee(()=>{et(),Le(tt(),{instantly:!0})})}),Be(()=>e.type,()=>{Ee(()=>{const a=R.value;a&&(a.classList.add("transition-disabled"),et(),a.offsetWidth,a.classList.remove("transition-disabled"))})});const _e=O(!1);function Tt(a){const{target:o,contentRect:{width:c,height:w}}=a,H=o.parentElement.parentElement.offsetWidth,q=o.parentElement.parentElement.offsetHeight,V=g.value;if(!_e.value)V==="top"||V==="bottom"?H<c&&(_e.value=!0):q<w&&(_e.value=!0);else{const{value:Z}=N;if(!Z)return;V==="top"||V==="bottom"?H-c>Z.$el.offsetWidth&&(_e.value=!1):q-w>Z.$el.offsetHeight&&(_e.value=!1)}Le(T.value?.$el||null)}const $t=Ke(Tt,64);function Pt(){const{onAdd:a}=e;a&&a()}const He=O(!1);function tt(){const a=g.value;return(a==="top"||a==="bottom"?T.value?.$el:E.value)||null}function Le(a,o={instantly:!1}){if(!a)return;const c=o.instantly?B.value:null;c&&c.classList.add("transition-disabled");const w=1,H=g.value;if(H==="top"||H==="bottom"){const{scrollLeft:q,scrollWidth:V,offsetWidth:Z}=a,pe=Math.abs(q);Y.value=pe<=w,J.value=pe+Z>=V-w,He.value=Z<V-w}else{const{scrollTop:q,scrollHeight:V,offsetHeight:Z}=a;Y.value=q<=w,J.value=q+Z>=V-w,He.value=Z<V-w}c&&(c.offsetWidth,c.classList.remove("transition-disabled"))}const Bt=Ke(a=>{Le(a.target)},64);Ta(Qe,{triggerRef:fe(e,"trigger"),tabStyleRef:fe(e,"tabStyle"),tabClassRef:fe(e,"tabClass"),addTabStyleRef:fe(e,"addTabStyle"),addTabClassRef:fe(e,"addTabClass"),paneClassRef:fe(e,"paneClass"),paneStyleRef:fe(e,"paneStyle"),mergedClsPrefixRef:i,typeRef:fe(e,"type"),closableRef:fe(e,"closable"),valueRef:k,tabChangeIdRef:m,onBeforeLeaveRef:fe(e,"onBeforeLeave"),activateTab:l,handleClose:d,handleAdd:Pt}),La(()=>{D(),oe()}),wa(()=>{const{value:a}=B;if(!a)return;const{value:o}=i,c=`${o}-tabs-nav-scroll-wrapper--shadow-start`,w=`${o}-tabs-nav-scroll-wrapper--shadow-end`;Y.value?a.classList.remove(c):a.classList.add(c),J.value?a.classList.remove(w):a.classList.add(w)});const Et={syncBarPosition:()=>{D()},scrollToCurrentTab:()=>{oe()}},Ot=()=>{Re({transitionDisabled:!0})},at=ve(()=>{const{value:a}=ce,{type:o}=e,c=`${a}${{card:"Card",bar:"Bar",line:"Line",segment:"Segment"}[o]}`,{self:{barColor:w,closeIconColor:H,closeIconColorHover:q,closeIconColorPressed:V,tabColor:Z,tabBorderColor:pe,paneTextColor:Lt,tabFontWeight:It,tabBorderRadius:Wt,tabFontWeightActive:At,colorSegment:jt,fontWeightStrong:Nt,tabColorSegment:Ut,closeSize:Dt,closeIconSize:Ht,closeColorHover:Mt,closeColorPressed:Ft,closeBorderRadius:Vt,[de("panePadding",a)]:Ie,[de("tabPadding",c)]:Jt,[de("tabPaddingVertical",c)]:qt,[de("tabGap",c)]:Kt,[de("tabGap",`${c}Vertical`)]:Gt,[de("tabTextColor",o)]:Xt,[de("tabTextColorActive",o)]:Yt,[de("tabTextColorHover",o)]:Zt,[de("tabTextColorDisabled",o)]:Qt,[de("tabFontSize",a)]:ea},common:{cubicBezierEaseInOut:ta}}=v.value;return{"--n-bezier":ta,"--n-color-segment":jt,"--n-bar-color":w,"--n-tab-font-size":ea,"--n-tab-text-color":Xt,"--n-tab-text-color-active":Yt,"--n-tab-text-color-disabled":Qt,"--n-tab-text-color-hover":Zt,"--n-pane-text-color":Lt,"--n-tab-border-color":pe,"--n-tab-border-radius":Wt,"--n-close-size":Dt,"--n-close-icon-size":Ht,"--n-close-color-hover":Mt,"--n-close-color-pressed":Ft,"--n-close-border-radius":Vt,"--n-close-icon-color":H,"--n-close-icon-color-hover":q,"--n-close-icon-color-pressed":V,"--n-tab-color":Z,"--n-tab-font-weight":It,"--n-tab-font-weight-active":At,"--n-tab-padding":Jt,"--n-tab-padding-vertical":qt,"--n-tab-gap":Kt,"--n-tab-gap-vertical":Gt,"--n-pane-padding-left":We(Ie,"left"),"--n-pane-padding-right":We(Ie,"right"),"--n-pane-padding-top":We(Ie,"top"),"--n-pane-padding-bottom":We(Ie,"bottom"),"--n-font-weight-strong":Nt,"--n-tab-color-segment":Ut}}),rt=x?kt("tabs",ve(()=>`${ce.value[0]}${e.type[0]}`),at,e):void 0;return{mergedClsPrefix:i,mergedValue:k,renderedNames:new Set,segmentCapsuleElRef:se,tabsPaneWrapperRef:ge,tabsElRef:y,selfElRef:R,barElRef:W,addTabInstRef:N,xScrollInstRef:T,scrollWrapperElRef:B,addTabFixed:_e,tabWrapperStyle:M,handleNavResize:zt,mergedSize:ce,handleScroll:Bt,handleTabsResize:$t,cssVars:x?void 0:at,themeClass:rt?.themeClass,animationDirection:Se,renderNameListRef:Ce,yScrollElRef:E,handleSegmentResize:Ot,onAnimationBeforeLeave:ze,onAnimationEnter:Te,onAnimationAfterEnter:$e,onRender:rt?.onRender,startReachedRef:Y,endReachedRef:J,isOverflow:He,handleButtonClick:L,mergedTheme:v,rtlEnabled:C,mergedPlacement:g,...Et}},render(){const{mergedClsPrefix:e,type:r,mergedPlacement:i,addTabFixed:x,addable:h,mergedSize:f,renderNameListRef:C,onRender:g,paneWrapperClass:v,paneWrapperStyle:y,startReachedRef:R,endReachedRef:W,isOverflow:B,showScrollButton:N,handleButtonClick:T,mergedTheme:E,rtlEnabled:Y,$slots:{default:J,prefix:A,suffix:ce}}=this;g?.();const te=J?Me(J()).filter($=>$.type.__TAB_PANE__===!0):[],ee=J?Me(J()).filter($=>$.type.__TAB__===!0):[],k=!ee.length,m=r==="card",M=r==="segment",j=!m&&!M&&this.justifyContent;C.value=[];const G=()=>{const $=(s(),p("div",{style:ye(this.tabWrapperStyle),class:P(`${e}-tabs-wrapper`)},[j?z(()=>null):(s(),p("div",{key:1,class:P(`${e}-tabs-scroll-padding`),style:ye(i==="top"||i==="bottom"?{width:`${this.tabsPadding}px`}:{height:`${this.tabsPadding}px`})},null,6)),k?(s(),p(Q,{key:2},[z(()=>te.map((D,ne)=>(C.value.push(D.props.name),Ge((s(),I(Ye,Ne(D.props,{internalCreatedByPane:!0,internalLeftPadded:ne!==0&&(!j||j==="center"||j==="start"||j==="end")}),nt(D.children?{default:D.children.tab}:void 0),1040,["internalLeftPadded"]))))))],64)):(s(),p(Q,{key:3},[z(()=>ee.map((D,ne)=>(C.value.push(D.props.name),Ge(ne!==0&&!j?gt(D):D))))],64)),!x&&h&&m?(s(),p(Q,{key:4},[z(()=>ht(h,(k?te.length:ee.length)!==0))],64)):z(()=>null),j?z(()=>null):(s(),p("div",{key:7,class:P(`${e}-tabs-scroll-padding`),style:ye({width:`${this.tabsPadding}px`})},null,6)),m?z(()=>null):(s(),p("div",{key:9,ref:"barElRef",class:P(`${e}-tabs-bar`)},null,2))],6));return s(),p("div",{ref:"tabsElRef",class:P(`${e}-tabs-nav-scroll-content`)},[m&&h?(s(),I(Fe,{key:0,onResize:this.handleTabsResize},{default:()=>$},1032,["onResize"])):(s(),p(Q,{key:1},[z(()=>$)],64)),m?(s(),p("div",{key:2,class:P(`${e}-tabs-pad`)},null,2)):z(()=>null)],2)},F=M?"top":i;return s(),p("div",{ref:"selfElRef",class:P([`${e}-tabs`,this.themeClass,`${e}-tabs--${r}-type`,`${e}-tabs--${f}-size`,j&&`${e}-tabs--flex`,`${e}-tabs--${F}`,Y&&`${e}-tabs--rtl`]),style:ye(this.cssVars)},[b("div",{class:P([`${e}-tabs-nav--${r}-type`,`${e}-tabs-nav--${F}`,`${e}-tabs-nav`])},[z(()=>Xe(A,$=>$&&(s(),p("div",{class:P(`${e}-tabs-nav__prefix`)},[z(()=>$)],2)))),M?(s(),I(Fe,{key:0,onResize:this.handleSegmentResize},{default:()=>(s(),p("div",{class:P(`${e}-tabs-rail`),ref:"tabsElRef"},[b("div",{class:P(`${e}-tabs-capsule`),ref:"segmentCapsuleElRef"},[b("div",{class:P(`${e}-tabs-wrapper`)},[b("div",{class:P(`${e}-tabs-tab`)},null,2)],2)],2),k?(s(),p(Q,{key:0},[z(()=>te.map(($,D)=>(C.value.push($.props.name),s(),I(Ye,Ne($.props,{internalCreatedByPane:!0,internalLeftPadded:D!==0}),nt($.children?{default:$.children.tab}:void 0),1040,["internalLeftPadded"]))))],64)):(s(),p(Q,{key:1},[z(()=>ee.map(($,D)=>(C.value.push($.props.name),D===0?$:gt($))))],64))],2))},1032,["onResize"])):(s(),p(Q,{key:1},[z(()=>N&&B&&(s(),I(vt,{mergedClsPrefix:e,type:"prev",vertical:F==="left"||F==="right",disabled:R,rtl:!!Y,theme:E.peers.Button,themeOverrides:E.peerOverrides.Button,onClick:T},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"]))),(s(),I(Fe,{onResize:this.handleNavResize},{default:()=>(s(),p("div",{class:P(`${e}-tabs-nav-scroll-wrapper`),ref:"scrollWrapperElRef"},[["top","bottom"].includes(F)?(s(),I(Ya,{key:0,ref:"xScrollInstRef",onScroll:this.handleScroll},{default:G},1032,["onScroll"])):(s(),p("div",{key:1,class:P(`${e}-tabs-nav-y-scroll`),onScroll:this.handleScroll,ref:"yScrollElRef"},[z(()=>G())],42,["onScroll"]))],2))},1032,["onResize"])),z(()=>N&&B&&(s(),I(vt,{mergedClsPrefix:e,type:"next",vertical:F==="left"||F==="right",disabled:W,rtl:!!Y,theme:E.peers.Button,themeOverrides:E.peerOverrides.Button,onClick:T},null,8,["mergedClsPrefix","vertical","disabled","rtl","theme","themeOverrides","onClick"])))],64)),x&&h&&m?(s(),p(Q,{key:2},[z(()=>ht(h,!0))],64)):z(()=>null),z(()=>Xe(ce,$=>$&&(s(),p("div",{class:P(`${e}-tabs-nav__suffix`)},[z(()=>$)],2))))],2),z(()=>k&&(this.animated&&(F==="top"||F==="bottom")?(s(),p("div",{key:1,ref:"tabsPaneWrapperRef",style:ye(y),class:P([`${e}-tabs-pane-wrapper`,v])},[z(()=>pt(te,this.mergedValue,this.renderedNames,this.onAnimationBeforeLeave,this.onAnimationEnter,this.onAnimationAfterEnter,this.animationDirection))],6)):pt(te,this.mergedValue,this.renderedNames)))],6)}});function pt(e,r,i,x,h,f,C){const g=[];return e.forEach(v=>{const{name:y,displayDirective:R,"display-directive":W}=v.props,B=T=>R===T||W===T,N=r===y;if(v.key!==void 0&&(v.key=y),N||B("show")||B("show:lazy")&&i.has(y)){i.has(y)||i.add(y);const T=!B("if");g.push(T?Ca(v,[[Ra,N]]):v)}}),C?(s(),I(_a,{name:`${C}-transition`,onBeforeLeave:x,onEnter:h,onAfterEnter:f},{default:()=>g},1032,["name","onBeforeLeave","onEnter","onAfterEnter"])):g}function ht(e,r){return s(),I(Ye,{ref:"addTabInstRef",key:"__addable",name:"__addable",internalCreatedByPane:!0,internalAddable:!0,internalLeftPadded:r,disabled:typeof e=="object"&&e.disabled},null,8,["internalLeftPadded","disabled"])}function gt(e){const r=za(e);return r.props?r.props.internalLeftPadded=!0:r.props={internalLeftPadded:!0},r}function Ge(e){return Array.isArray(e.dynamicProps)?e.dynamicProps.includes("internalLeftPadded")||e.dynamicProps.push("internalLeftPadded"):e.dynamicProps=["internalLeftPadded"],e}const br={class:"page-view"},fr={class:"page-intro config-page-intro"},vr={class:"config-toolbar"},pr={class:"config-meta-row"},hr={key:0,class:"muted"},gr={key:0,class:"revision-panel"},mr={key:0,class:"muted"},yr={class:"config-editor-layout"},xr={class:"config-groups"},kr=["onClick"],wr={class:"config-editor"},Cr={class:"editor-view-tabs"},Sr={key:0,class:"field-list"},Rr={class:"field-copy"},_r={key:0},zr={class:"field-control"},Tr={class:"save-bar"},$r={class:"system-settings-panel"},Pr={class:"field-copy"},Br={class:"field-control"},Er={class:"save-bar"},Nr=xe({__name:"ConfigView",setup(e){const r=$a(),i=Pa(),x=O("profile"),h=O("visual"),f=O(""),C=O("ai"),g=O(""),v=O(""),y=lt({}),R=lt({log_level:"info",request_timeout_seconds:300}),W=O([]),B=O(!1),N=O("{}"),T=O(!1),E=O(!1),Y=O(null),J=[{key:"ai",label:"AI 与模型"},{key:"persona",label:"人格"},{key:"context",label:"上下文管理"},{key:"agent",label:"Agent 执行"},{key:"workspace",label:"项目能力"},{key:"message",label:"消息输出"},{key:"memory",label:"长期记忆"}],A=ve(()=>r.configProfiles.find(l=>l.id===f.value)||r.defaultProfile),ce=ve(()=>r.configProfiles.map(l=>({label:`${l.name}${l.is_default?" · 默认":""}`,value:l.id}))),te=ve(()=>(r.configSchema.fields||[]).filter(l=>!g.value.trim()&&l.group!==C.value||!j(l)?!1:g.value.trim()?[l.label,l.key,l.help].filter(Boolean).join(" ").toLocaleLowerCase().includes(g.value.trim().toLocaleLowerCase()):!0));function ee(l){return JSON.parse(JSON.stringify(l??{}))}function k(l){return Object.prototype.hasOwnProperty.call(y,l.key)?y[l.key]:l.default}function m(l){const t=k(l);return typeof t=="string"?t:t==null?"":String(t)}function M(l){const t=k(l);return typeof t=="number"?t:Number(t||0)}function j(l){return Object.entries(l.display_if||{}).every(([t,d])=>{const L=r.configSchema.fields.find(be=>be.key===t),ue=L?k(L):y[t];return JSON.stringify(ue)===JSON.stringify(d)})}function G(l){return l.option_source==="providers"?[{label:"沿用供应商注册表默认",value:""},...r.providers.map(t=>({label:t.name,value:t.id}))]:(l.options||[]).map(t=>({label:t.label,value:t.value}))}function F(l,t){y[l.key]=t,B.value=!0}function $(l){Object.keys(y).forEach(t=>delete y[t]),Object.assign(y,ee(l?.values||{})),v.value=l?.name||"",N.value=JSON.stringify(y,null,2),B.value=!1}async function D(l){B.value&&!window.confirm("当前配置有未保存修改，确认放弃吗？")||(f.value=l,$(r.configProfiles.find(t=>t.id===l)),await ne())}async function ne(){if(W.value=[],!!f.value)try{W.value=await Ba(f.value)}catch(l){i.error(l instanceof Error?l.message:"读取修订历史失败")}}async function oe(l=f.value){await r.loadConfig(),f.value=l||r.defaultProfileID||r.configProfiles[0]?.id||"",$(r.configProfiles.find(t=>t.id===f.value)),R.log_level=r.systemSettings.log_level,R.request_timeout_seconds=r.systemSettings.request_timeout_seconds,await ne()}async function ge(){if(f.value){if(h.value==="json")try{const l=JSON.parse(N.value);Object.keys(y).forEach(t=>delete y[t]),Object.assign(y,l)}catch(l){i.error(`JSON 格式无效：${l instanceof Error?l.message:"无法解析"}`);return}T.value=!0;try{const l={id:f.value,name:v.value.trim(),values:y},t=await he(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/validate`,{method:"POST",body:JSON.stringify(l)});if(!t.valid)throw new Error(t.error||"配置校验失败");await he(`/api/v1/config-profiles/${encodeURIComponent(f.value)}`,{method:"PUT",body:JSON.stringify(l)}),await oe(f.value),i.success("配置已保存并立即生效")}catch(l){i.error(l instanceof Error?l.message:"保存配置失败")}finally{T.value=!1}}}async function ke(){const l=window.prompt("请输入配置文件名称","新配置");if(l?.trim())try{const t=await he("/api/v1/config-profiles",{method:"POST",body:JSON.stringify({name:l.trim(),values:{}})});await oe(t.id),i.success("配置文件已创建")}catch(t){i.error(t instanceof Error?t.message:"创建配置文件失败")}}async function le(){if(!f.value)return;const l=window.prompt("请输入副本名称",`${A.value?.name||"配置"} 副本`);if(l?.trim())try{const t=await he(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/copy`,{method:"POST",body:JSON.stringify({name:l.trim()})});await oe(t.id),i.success("配置副本已创建")}catch(t){i.error(t instanceof Error?t.message:"复制配置失败")}}async function ze(){if(f.value)try{await he(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/default`,{method:"POST",body:"{}"}),await oe(f.value),i.success("已切换系统默认配置")}catch(l){i.error(l instanceof Error?l.message:"切换默认配置失败")}}async function Te(){if(!(!A.value||A.value.is_default)&&window.confirm(`彻底删除“${A.value.name}”及全部修订历史？`))try{await he(`/api/v1/config-profiles/${encodeURIComponent(A.value.id)}`,{method:"DELETE"}),await oe(),i.success("配置文件已删除")}catch(l){i.error(l instanceof Error?l.message:"删除配置失败")}}async function $e(){if(f.value)try{const l=await he(`/api/v1/config-profiles/${encodeURIComponent(f.value)}/export`),t=document.createElement("a");t.href=URL.createObjectURL(new Blob([JSON.stringify(l,null,2)],{type:"application/json"})),t.download=`${f.value}.json`,t.click(),URL.revokeObjectURL(t.href)}catch(l){i.error(l instanceof Error?l.message:"导出配置失败")}}async function Ce(l){const t=l.target,d=t.files?.[0];if(t.value="",!!d)try{const L=JSON.parse(await d.text()),ue={version:L.version,id:L.id,name:L.name,values:L.values||{},overwrite:!1};let be;try{be=await he("/api/v1/config-profiles/import",{method:"POST",body:JSON.stringify(ue)})}catch(se){if(!(se instanceof Error)||!/已存在|冲突/.test(se.message)||!window.confirm("配置文件已存在，确认覆盖同 ID 配置吗？"))throw se;ue.overwrite=!0,be=await he("/api/v1/config-profiles/import",{method:"POST",body:JSON.stringify(ue)})}await oe(be.id),i.success("配置已导入")}catch(L){i.error(L instanceof Error?L.message:"导入配置失败")}}async function Se(){try{const l=await he("/api/v1/system-settings",{method:"PUT",body:JSON.stringify(R)});r.systemSettings.log_level=l.log_level,r.systemSettings.request_timeout_seconds=l.request_timeout_seconds,i.success("系统设置已保存并立即生效")}catch(l){i.error(l instanceof Error?l.message:"保存系统设置失败")}}return St(async()=>{await r.loadAll(),f.value=r.defaultProfileID||r.configProfiles[0]?.id||"",$(A.value),R.log_level=r.systemSettings.log_level,R.request_timeout_seconds=r.systemSettings.request_timeout_seconds,await ne()}),(l,t)=>(s(),p("div",br,[b("div",fr,[t[14]||(t[14]=b("div",null,[b("p",{class:"eyebrow"},"CONFIG CENTER"),b("h2",null,"配置文件"),b("p",null,"普通配置和系统设置共用同一套 Schema 表单；每次保存都会先校验，成功后立即应用到下一轮请求。")],-1)),K(_(ot),null,{default:X(()=>[K(_(ie),{secondary:"",onClick:$e},{default:X(()=>[...t[12]||(t[12]=[ae("导出配置",-1)])]),_:1}),K(_(ie),{secondary:"",onClick:t[0]||(t[0]=d=>Y.value?.click())},{default:X(()=>[...t[13]||(t[13]=[ae("导入配置",-1)])]),_:1}),b("input",{ref_key:"importInput",ref:Y,type:"file",accept:"application/json,.json",class:"visually-hidden",onChange:Ce},null,544)]),_:1})]),K(_(Ea),{class:"config-card",bordered:!1},{default:X(()=>[K(_(ur),{value:x.value,"onUpdate:value":t[11]||(t[11]=d=>x.value=d),type:"segment",animated:""},{default:X(()=>[K(_(ft),{name:"profile",tab:"普通配置"},{default:X(()=>[b("div",vr,[K(_(Je),{class:"profile-select",value:f.value,options:ce.value,"onUpdate:value":D},null,8,["value","options"]),K(_(Pe),{value:g.value,"onUpdate:value":t[1]||(t[1]=d=>g.value=d),clearable:"",placeholder:"搜索设置名称、说明或配置键",class:"config-search"},null,8,["value"]),K(_(ot),null,{default:X(()=>[K(_(ie),{secondary:"",onClick:ke},{default:X(()=>[...t[15]||(t[15]=[ae("新建",-1)])]),_:1}),K(_(ie),{secondary:"",disabled:!A.value,onClick:le},{default:X(()=>[...t[16]||(t[16]=[ae("复制",-1)])]),_:1},8,["disabled"]),K(_(ie),{secondary:"",disabled:!A.value||A.value.is_default,onClick:ze},{default:X(()=>[...t[17]||(t[17]=[ae("设为默认",-1)])]),_:1},8,["disabled"]),K(_(ie),{tertiary:"",type:"error",disabled:!A.value||A.value.is_default,onClick:Te},{default:X(()=>[...t[18]||(t[18]=[ae("删除",-1)])]),_:1},8,["disabled"])]),_:1})]),b("div",pr,[b("div",null,[K(_(Pe),{value:v.value,"onUpdate:value":[t[2]||(t[2]=d=>v.value=d),t[3]||(t[3]=d=>B.value=!0)],placeholder:"配置名称"},null,8,["value"]),A.value?(s(),p("span",hr,"当前修订 v"+re(A.value.revision)+re(A.value.is_default?" · 系统默认":""),1)):me("",!0)]),K(_(ie),{text:"",onClick:t[4]||(t[4]=d=>E.value=!E.value)},{default:X(()=>[ae(re(E.value?"收起修订历史":"查看修订历史"),1)]),_:1})]),E.value?(s(),p("div",gr,[(s(!0),p(Q,null,Ae(W.value,d=>(s(),p("div",{key:`${d.profile_id}-${d.revision}`,class:"revision-row"},[b("strong",null,"v"+re(d.revision),1),b("span",null,re(d.created_at?new Date(d.created_at).toLocaleString():"未知时间"),1),d.revision===A.value?.revision?(s(),I(_(Ve),{key:0,size:"small",type:"success"},{default:X(()=>[...t[19]||(t[19]=[ae("当前",-1)])]),_:1})):me("",!0)]))),128)),W.value.length?me("",!0):(s(),p("span",mr,"暂无修订历史"))])):me("",!0),b("div",yr,[b("aside",xr,[(s(),p(Q,null,Ae(J,d=>b("button",{key:d.key,type:"button",class:st({active:C.value===d.key}),onClick:L=>C.value=d.key},re(d.label),11,kr)),64))]),b("section",wr,[b("div",Cr,[K(_(ie),{size:"small",type:h.value==="visual"?"primary":"default",onClick:t[5]||(t[5]=d=>h.value="visual")},{default:X(()=>[...t[20]||(t[20]=[ae("可视化",-1)])]),_:1},8,["type"]),K(_(ie),{size:"small",type:h.value==="json"?"primary":"default",onClick:t[6]||(t[6]=d=>{N.value=JSON.stringify(y,null,2),h.value="json"})},{default:X(()=>[...t[21]||(t[21]=[ae("JSON",-1)])]),_:1},8,["type"])]),h.value==="visual"?(s(),p("div",Sr,[(s(!0),p(Q,null,Ae(te.value,d=>(s(),p("div",{key:d.key,class:"config-field-row"},[b("div",Rr,[b("strong",null,re(d.label),1),b("code",null,re(d.key),1),d.help?(s(),p("span",_r,re(d.help),1)):me("",!0),d.restart_required?(s(),I(_(Ve),{key:1,size:"small",type:"warning"},{default:X(()=>[...t[22]||(t[22]=[ae("需重启",-1)])]),_:1})):me("",!0)]),b("div",zr,[d.type==="boolean"?(s(),I(_(or),{key:0,checked:k(d)===!0,"onUpdate:checked":L=>F(d,L)},null,8,["checked","onUpdate:checked"])):d.type==="select"?(s(),I(_(Je),{key:1,value:String(k(d)??""),options:G(d),"onUpdate:value":L=>F(d,L)},null,8,["value","options","onUpdate:value"])):d.type==="integer"||d.type==="number"?(s(),I(_(ct),{key:2,value:M(d),min:d.min,max:d.max,step:d.type==="number"?.01:1,"show-button":!1,"onUpdate:value":L=>F(d,L)},null,8,["value","min","max","step","onUpdate:value"])):d.type==="textarea"?(s(),I(_(Pe),{key:3,type:"textarea",value:m(d),autosize:{minRows:3,maxRows:8},"onUpdate:value":L=>F(d,L)},null,8,["value","onUpdate:value"])):(s(),I(_(Pe),{key:4,value:m(d),type:d.secret?"password":"text","onUpdate:value":L=>F(d,L)},null,8,["value","type","onUpdate:value"]))])]))),128)),te.value.length?me("",!0):(s(),I(_(aa),{key:0,description:"当前没有匹配的设置"}))])):(s(),I(_(Pe),{key:1,value:N.value,"onUpdate:value":[t[7]||(t[7]=d=>N.value=d),t[8]||(t[8]=d=>B.value=!0)],type:"textarea",autosize:{minRows:18,maxRows:30},class:"json-editor"},null,8,["value"])),b("div",Tr,[b("span",{class:st({dirty:B.value})},re(B.value?"有未保存修改":"配置已同步"),3),K(_(ie),{type:"primary",loading:T.value,disabled:!A.value,onClick:ge},{default:X(()=>[...t[23]||(t[23]=[ae("保存当前修订",-1)])]),_:1},8,["loading","disabled"])])])])]),_:1}),K(_(ft),{name:"system",tab:"系统设置"},{default:X(()=>[b("div",$r,[t[27]||(t[27]=b("div",{class:"section-heading-row"},[b("div",null,[b("h3",null,"系统设置"),b("p",null,"日志等级和全局请求超时保存后立即生效，覆盖 WebUI、API 和机器人消息。")])],-1)),(s(!0),p(Q,null,Ae(_(r).systemSettings.schema||[],d=>(s(),p("div",{key:d.key,class:"config-field-row"},[b("div",Pr,[b("strong",null,re(d.label),1),b("code",null,"system."+re(d.key),1),b("span",null,re(d.help),1),d.restart_required?(s(),I(_(Ve),{key:0,size:"small",type:"warning"},{default:X(()=>[...t[24]||(t[24]=[ae("需重启",-1)])]),_:1})):me("",!0)]),b("div",Br,[d.type==="select"?(s(),I(_(Je),{key:0,value:R.log_level,"onUpdate:value":t[9]||(t[9]=L=>R.log_level=L),options:d.options||[]},null,8,["value","options"])):(s(),I(_(ct),{key:1,value:R.request_timeout_seconds,"onUpdate:value":t[10]||(t[10]=L=>R.request_timeout_seconds=L),min:d.min,max:d.max,"show-button":!1},null,8,["value","min","max"]))])]))),128)),b("div",Er,[t[26]||(t[26]=b("span",null,"保存后立即应用",-1)),K(_(ie),{type:"primary",onClick:Se},{default:X(()=>[...t[25]||(t[25]=[ae("保存系统设置",-1)])]),_:1})])])]),_:1})]),_:1},8,["value"])]),_:1})]))}});export{Nr as default};
