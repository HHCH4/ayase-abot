import{_ as V,a as k,a6 as l,v as r,a7 as h,a8 as s,aw as se,ax as be,ay as he,d as ue,x as F,g as _,c as B,a0 as z,z as u,b as ke,az as fe,y as ve,a1 as xe,A as me,r as N,aA as ge,aa as pe,B as Ce,n as P,aB as ye,aC as ze,ae as E,G as we,$ as Re}from"./index-COCbPh4K.js";import{r as Se,o as _e,a as Be,b as T}from"./use-message-DE8MVSa4.js";import{a as De}from"./Space-BNhAHqdN.js";var $e=()=>(()=>{const e=V("75be776d8875fa17");return e[0]||(e[0]=k("svg",{viewBox:"0 0 64 64",class:"check-icon"},[k("path",{d:"M50.42,16.76L22.34,39.45l-8.1-11.46c-1.12-1.58-3.3-1.96-4.88-0.84c-1.58,1.12-1.95,3.3-0.84,4.88l10.26,14.51  c0.56,0.79,1.42,1.31,2.38,1.45c0.16,0.02,0.32,0.03,0.48,0.03c0.8,0,1.57-0.27,2.2-0.78l30.99-25.03c1.5-1.21,1.74-3.42,0.52-4.92  C54.13,15.78,51.93,15.55,50.42,16.76z"})],-1))})(),Me=()=>(()=>{const e=V("c6eed899356c8404");return e[0]||(e[0]=k("svg",{viewBox:"0 0 100 100",class:"line-icon"},[k("path",{d:"M80.2,55.5H21.4c-2.8,0-5.1-2.5-5.1-5.5l0,0c0-3,2.3-5.5,5.1-5.5h58.7c2.8,0,5.1,2.5,5.1,5.5l0,0C85.2,53.1,82.9,55.5,80.2,55.5z"})],-1))})(),Te=l([r("checkbox",`
 font-size: var(--n-font-size);
 outline: none;
 cursor: pointer;
 display: inline-flex;
 flex-wrap: nowrap;
 align-items: flex-start;
 word-break: break-word;
 line-height: var(--n-size);
 --n-merged-color-table: var(--n-color-table);
 `,[h("show-label","line-height: var(--n-label-line-height);"),l("&:hover",[r("checkbox-box",[s("border","border: var(--n-border-checked);")])]),l("&:focus:not(:active)",[r("checkbox-box",[s("border",`
 border: var(--n-border-focus);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),h("inside-table",[r("checkbox-box",`
 background-color: var(--n-merged-color-table);
 `)]),h("checked",[r("checkbox-box",`
 background-color: var(--n-color-checked);
 `,[r("checkbox-icon",[l(".check-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),h("indeterminate",[r("checkbox-box",[r("checkbox-icon",[l(".check-icon",`
 opacity: 0;
 transform: scale(.5);
 `),l(".line-icon",`
 opacity: 1;
 transform: scale(1);
 `)])])]),h("checked, indeterminate",[l("&:focus:not(:active)",[r("checkbox-box",[s("border",`
 border: var(--n-border-checked);
 box-shadow: var(--n-box-shadow-focus);
 `)])]),r("checkbox-box",`
 background-color: var(--n-color-checked);
 border-left: 0;
 border-top: 0;
 `,[s("border",{border:"var(--n-border-checked)"})])]),h("disabled",{cursor:"not-allowed"},[h("checked",[r("checkbox-box",`
 background-color: var(--n-color-disabled-checked);
 `,[s("border",{border:"var(--n-border-disabled-checked)"}),r("checkbox-icon",[l(".check-icon, .line-icon",{fill:"var(--n-check-mark-color-disabled-checked)"})])])]),r("checkbox-box",`
 background-color: var(--n-color-disabled);
 `,[s("border",`
 border: var(--n-border-disabled);
 `),r("checkbox-icon",[l(".check-icon, .line-icon",`
 fill: var(--n-check-mark-color-disabled);
 `)])]),s("label",`
 color: var(--n-text-color-disabled);
 `)]),r("checkbox-box-wrapper",`
 position: relative;
 width: var(--n-size);
 flex-shrink: 0;
 flex-grow: 0;
 user-select: none;
 -webkit-user-select: none;
 `),r("checkbox-box",`
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
 `,[s("border",`
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
 `),r("checkbox-icon",`
 display: flex;
 align-items: center;
 justify-content: center;
 position: absolute;
 left: 1px;
 right: 1px;
 top: 1px;
 bottom: 1px;
 `,[l(".check-icon, .line-icon",`
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
 `),se({left:"1px",top:"1px"})])]),s("label",`
 color: var(--n-text-color);
 transition: color .3s var(--n-bezier);
 user-select: none;
 -webkit-user-select: none;
 padding: var(--n-label-padding);
 font-weight: var(--n-label-font-weight);
 `,[l("&:empty",{display:"none"})])]),be(r("checkbox",`
 --n-merged-color-table: var(--n-color-table-modal);
 `)),he(r("checkbox",`
 --n-merged-color-table: var(--n-color-table-popover);
 `))]);const Ve=["id"],Ke=["tabindex","aria-checked","aria-labelledby","onKeyup","onKeydown","onClick"],Ie={...F.props,size:String,checked:{type:[Boolean,String,Number],default:void 0},defaultChecked:{type:[Boolean,String,Number],default:!1},value:[String,Number],disabled:{type:Boolean,default:void 0},indeterminate:Boolean,label:String,focusable:{type:Boolean,default:!0},checkedValue:{type:[Boolean,String,Number],default:!0},uncheckedValue:{type:[Boolean,String,Number],default:!1},"onUpdate:checked":[Function,Array],onUpdateChecked:[Function,Array],privateInsideTable:Boolean,onChange:[Function,Array]};var Ue=ue({name:"Checkbox",props:Ie,setup(e){const a=xe(Ne,null),f=N(null),{mergedClsPrefixRef:v,inlineThemeDisabled:w,mergedRtlRef:D,mergedComponentPropsRef:R}=me(e),g=N(e.defaultChecked),c=we(e,"checked"),$=De(c,g),b=ge(()=>{if(a){const o=a.valueSetRef.value;return o&&e.value!==void 0?o.has(e.value):!1}else return $.value===e.checkedValue}),p=Be(e,{mergedSize(o){const{size:d}=e;if(d!==void 0)return d;if(a){const{value:n}=a.mergedSizeRef;if(n!==void 0)return n}if(o){const{mergedSize:n}=o;if(n!==void 0)return n.value}const i=R?.value?.Checkbox?.size;return i||"medium"},mergedDisabled(o){const{disabled:d}=e;if(d!==void 0)return d;if(a){if(a.disabledRef.value)return!0;const{maxRef:{value:i},checkedCountRef:n}=a;if(i!==void 0&&n.value>=i&&!b.value)return!0;const{minRef:{value:x}}=a;if(x!==void 0&&n.value<=x&&b.value)return!0}return o?o.disabled.value:!1}}),{mergedDisabledRef:C,mergedSizeRef:y}=p,t=F("Checkbox","-checkbox",Te,ze,e,v);function S(o){if(a&&e.value!==void 0)a.toggleCheckbox(!b.value,e.value);else{const{onChange:d,"onUpdate:checked":i,onUpdateChecked:n}=e,{nTriggerFormInput:x,nTriggerFormChange:M}=p,m=b.value?e.uncheckedValue:e.checkedValue;i&&T(i,m,o),n&&T(n,m,o),d&&T(d,m,o),x(),M(),g.value=m}}function U(o){C.value||S(o)}function H(o){if(!C.value)switch(o.key){case" ":case"Enter":S(o)}}function j(o){o.key===" "&&o.preventDefault()}const A={focus:()=>{f.value?.focus()},blur:()=>{f.value?.blur()}},L=pe("Checkbox",D,v),K=P(()=>{const{value:o}=y,{common:{cubicBezierEaseInOut:d},self:{borderRadius:i,color:n,colorChecked:x,colorDisabled:M,colorTableHeader:m,colorTableHeaderModal:G,colorTableHeaderPopover:O,checkMarkColor:W,checkMarkColorDisabled:Y,border:q,borderFocus:J,borderDisabled:Q,borderChecked:X,boxShadowFocus:Z,textColor:ee,textColorDisabled:oe,checkMarkColorDisabledChecked:re,colorDisabledChecked:ce,borderDisabledChecked:ae,labelPadding:ne,labelLineHeight:le,labelFontWeight:te,[E("fontSize",o)]:de,[E("size",o)]:ie}}=t.value;return{"--n-label-line-height":le,"--n-label-font-weight":te,"--n-size":ie,"--n-bezier":d,"--n-border-radius":i,"--n-border":q,"--n-border-checked":X,"--n-border-focus":J,"--n-border-disabled":Q,"--n-border-disabled-checked":ae,"--n-box-shadow-focus":Z,"--n-color":n,"--n-color-checked":x,"--n-color-table":m,"--n-color-table-modal":G,"--n-color-table-popover":O,"--n-color-disabled":M,"--n-color-disabled-checked":ce,"--n-text-color":ee,"--n-text-color-disabled":oe,"--n-check-mark-color":W,"--n-check-mark-color-disabled":Y,"--n-check-mark-color-disabled-checked":re,"--n-font-size":de,"--n-label-padding":ne}}),I=w?Ce("checkbox",P(()=>y.value[0]),K,e):void 0;return Object.assign(p,A,{rtlEnabled:L,selfRef:f,mergedClsPrefix:v,mergedDisabled:C,renderedChecked:b,mergedTheme:t,labelId:ye(),handleClick:U,handleKeyUp:H,handleKeyDown:j,cssVars:w?void 0:K,themeClass:I?.themeClass,onRender:I?.onRender})},render(){const{$slots:e,renderedChecked:a,mergedDisabled:f,indeterminate:v,privateInsideTable:w,cssVars:D,labelId:R,label:g,mergedClsPrefix:c,focusable:$,handleKeyUp:b,handleKeyDown:p,handleClick:C}=this;this.onRender?.();const y=Se(e.default,t=>g||t?(_(),B("span",{key:1,class:u(`${c}-checkbox__label`),id:R},[z(()=>g||t)],10,Ve)):null);return(()=>{const t=V("70be6e74cd27cb50");return _(),B("div",{ref:"selfRef",class:u([`${c}-checkbox`,this.themeClass,this.rtlEnabled&&`${c}-checkbox--rtl`,a&&`${c}-checkbox--checked`,f&&`${c}-checkbox--disabled`,v&&`${c}-checkbox--indeterminate`,w&&`${c}-checkbox--inside-table`,y&&`${c}-checkbox--show-label`]),tabindex:f||!$?void 0:0,role:"checkbox","aria-checked":v?"mixed":a,"aria-labelledby":R,style:ve(D),onKeyup:b,onKeydown:p,onClick:C,onMousedown:t[0]||(t[0]=()=>{_e("selectstart",window,S=>{S.preventDefault()},{once:!0})})},[k("div",{class:u(`${c}-checkbox-box-wrapper`)},[t[1]||(t[1]=z(" ",-1)),k("div",{class:u(`${c}-checkbox-box`)},[ke(fe,null,{default:()=>this.indeterminate?(_(),B("div",{key:"indeterminate",class:u(`${c}-checkbox-icon`)},[z(()=>Me())],2)):(_(),B("div",{key:"check",class:u(`${c}-checkbox-icon`)},[z(()=>$e())],2))},1024),k("div",{class:u(`${c}-checkbox-box__border`)},null,2)],2)],2),z(()=>y)],46,Ke)})()}});const Ne=Re("n-checkbox-group");export{Ue as C};
